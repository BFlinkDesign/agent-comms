// Command fleetd records and answers what happened on which machine.
//
// It exists because git cannot answer that. A commit carries an author, a
// committer and a timezone offset; none of them identify a host. Measuring this
// fleet's own history found ten distinct commit identities for one person and no
// signal separating one workstation from another, so "which PC did I do that on"
// was never answerable after the fact. fleetd records it at the time.
//
// Three commands, deliberately:
//
//	fleetd host            what this machine is, and how confident that is
//	fleetd record ...      append one host-attributed record
//	fleetd where           per machine, what it was last doing
//
// Every command takes --json, so the same surface serves a person at a terminal
// and a program reading structured output. That is what a later MCP server would
// wrap; there is no second implementation to keep in sync.
//
// The journal directory is resolved from --dir, else COMMS_CHANNELS, else
// ./channels, matching how the rest of this bus is configured.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/cell"
	"github.com/BFlinkDesign/agent-comms/internal/hostid"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

const usage = `fleetd — record and answer what happened on which machine

usage:
  fleetd host   [--json] [--salt S]
  fleetd record [--json] [--dir D] [--salt S] --type T [--note N] [--repo R] [--branch B] [--agent A]
  fleetd where  [--json] [--dir D] [--limit N]

The journal directory is --dir, else $COMMS_CHANNELS, else ./channels.
--salt is --salt, else $FLEET_SALT. It separates this fleet's host digests from
any other and must be the same on every machine, or one machine will appear as
several. It is not a credential.
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "fleetd:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "host":
		return cmdHost(args[1:], stdout, stderr)
	case "record":
		return cmdRecord(args[1:], stdout, stderr)
	case "where":
		return cmdWhere(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// salt resolves the fleet salt. An empty salt still produces stable digests; it
// just does not separate this fleet from another using the same scheme, so the
// caller is told rather than silently given a weaker identity.
func resolveSalt(flagValue string, stderr io.Writer) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("FLEET_SALT"); v != "" {
		return v
	}
	fmt.Fprintln(stderr, "fleetd: warning: no --salt and no FLEET_SALT; host digests are unseparated")
	return ""
}

func resolveDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("COMMS_CHANNELS"); v != "" {
		return filepath.Join(v, "journal")
	}
	return filepath.Join("channels", "journal")
}

type hostOut struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	User   string `json:"user"`
	Source string `json:"source"`
	Stable bool   `json:"stable"`
}

func identity(salt string) hostOut {
	id := hostid.Derive(hostid.Options{Salt: salt})
	return hostOut{id.ID, id.Name, id.OS, id.Arch, id.User, id.Source, id.Stable}
}

func cmdHost(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	salt := fs.String("salt", "", "fleet salt (else $FLEET_SALT)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	h := identity(resolveSalt(*salt, stderr))
	if *asJSON {
		return writeJSON(stdout, h)
	}
	fmt.Fprintf(stdout, "id      %s\nname    %s\nos      %s/%s\nuser    %s\nsource  %s\n",
		h.ID, h.Name, h.OS, h.Arch, h.User, h.Source)
	if !h.Stable {
		// Say it plainly. An identity derived from a hostname changes when the
		// machine is renamed and collides with any other machine of that name, and
		// a reader who does not know that will over-trust the attribution.
		fmt.Fprintln(stdout, "\nwarning: no stable machine identifier was readable, so this id is derived")
		fmt.Fprintln(stdout, "from the hostname alone. It will change if the machine is renamed and it may")
		fmt.Fprintln(stdout, "collide with another machine of the same name.")
	}
	return nil
}

func cmdRecord(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	dir := fs.String("dir", "", "journal directory (else $COMMS_CHANNELS/journal, else ./channels/journal)")
	salt := fs.String("salt", "", "fleet salt (else $FLEET_SALT)")
	typ := fs.String("type", "", "record type, e.g. observation, handoff, note (required)")
	note := fs.String("note", "", "what happened, in your own words")
	repo := fs.String("repo", "", "repository the work was in")
	branch := fs.String("branch", "", "branch the work was on")
	agent := fs.String("agent", "", "which tool produced this, as name/role")
	at := fs.String("at", "", "RFC3339 timestamp (default: now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*typ) == "" {
		return errors.New("--type is required")
	}

	h := identity(resolveSalt(*salt, stderr))
	from := *agent
	if strings.TrimSpace(from) == "" {
		from = "fleetd/" + h.Name
	}

	ts := *at
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		return fmt.Errorf("--at %q is not an RFC3339 timestamp: %w", ts, err)
	}

	// Host facts travel with every record. That is the entire point: the record is
	// only useful later if it says where it came from, and a reader must be able to
	// see how much the attribution is worth without consulting anything else.
	data := map[string]cell.Value{
		"host.id":     cell.S(h.ID),
		"host.name":   cell.S(h.Name),
		"host.os":     cell.S(h.OS),
		"host.arch":   cell.S(h.Arch),
		"host.source": cell.S(h.Source),
		"host.stable": cell.B(h.Stable),
	}
	for k, v := range map[string]string{"note": *note, "repo": *repo, "branch": *branch, "user": h.User} {
		if strings.TrimSpace(v) != "" {
			data[k] = cell.S(v)
		}
	}

	c, err := cell.New(*typ, from, ts, "journal", data, nil, nil, 0)
	if err != nil {
		return err
	}

	store, err := journal.Open(resolveDir(*dir))
	if err != nil {
		return err
	}
	if err := store.Append(h.ID, c); err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(stdout, map[string]any{
			"id": c.ID, "host": h.ID, "file": filepath.Join(store.Dir(), journal.FileName(h.ID)+".jsonl"),
		})
	}
	fmt.Fprintf(stdout, "%s  recorded on %s\n", c.ID, h.Name)
	return nil
}

type whereEntry struct {
	Host     string `json:"host"`
	HostName string `json:"host_name"`
	OS       string `json:"os,omitempty"`
	Stable   bool   `json:"attribution_stable"`
	Records  int    `json:"records"`
	LastTS   string `json:"last_ts"`
	LastType string `json:"last_type,omitempty"`
	LastRepo string `json:"last_repo,omitempty"`
	LastNote string `json:"last_note,omitempty"`
}

func cmdWhere(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("where", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	dir := fs.String("dir", "", "journal directory")
	limit := fs.Int("limit", 3, "recent records to show per machine")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := journal.Open(resolveDir(*dir))
	if err != nil {
		return err
	}
	records, readErr := store.ReadAll()
	// A damaged journal is reported and the intact records are still used. Refusing
	// to answer at all because one machine's file was truncated would make a bad
	// day worse.
	if readErr != nil {
		fmt.Fprintln(stderr, "fleetd: warning:", readErr)
	}
	if len(records) == 0 {
		fmt.Fprintf(stdout, "no records in %s\n", store.Dir())
		if readErr != nil {
			return readErr
		}
		return nil
	}

	byHost := map[string][]journal.Record{}
	for _, r := range records {
		byHost[r.Host] = append(byHost[r.Host], r)
	}

	var out []whereEntry
	for host, recs := range byHost {
		// File order is the ordering primitive within a host: it is the order the
		// appends actually committed, and unlike a timestamp it cannot be wrong
		// because a writer's clock was. Across hosts there is deliberately no total
		// order; see the note printed below.
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].Line < recs[j].Line })
		last := recs[len(recs)-1]
		f := fieldsOf(last.Raw)
		out = append(out, whereEntry{
			Host: host, HostName: f["host.name"], OS: f["host.os"],
			Stable: f["host.stable"] == "true", Records: len(recs),
			LastTS: last.TS, LastType: last.Type, LastRepo: f["repo"], LastNote: f["note"],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })

	if *asJSON {
		return writeJSON(stdout, out)
	}
	for _, e := range out {
		name := e.HostName
		if name == "" {
			name = e.Host
		}
		flag := ""
		if !e.Stable {
			flag = "  [attribution: hostname only]"
		}
		fmt.Fprintf(stdout, "\n%s (%s, %s)%s\n", name, e.OS, plural(e.Records, "record"), flag)
		fmt.Fprintf(stdout, "  last  %s  %s\n", e.LastTS, e.LastType)
		if e.LastRepo != "" {
			fmt.Fprintf(stdout, "  repo  %s\n", e.LastRepo)
		}
		if e.LastNote != "" {
			fmt.Fprintf(stdout, "  note  %s\n", e.LastNote)
		}
		recs := byHost[e.Host]
		if n := *limit; n > 1 && len(recs) > 1 {
			fmt.Fprintln(stdout, "  before that:")
			for i := len(recs) - 2; i >= 0 && i > len(recs)-1-n; i-- {
				rf := fieldsOf(recs[i].Raw)
				fmt.Fprintf(stdout, "    %s  %-12s %s\n", recs[i].TS, recs[i].Type, rf["note"])
			}
		}
	}
	fmt.Fprintln(stdout, "\nRecords are ordered by each machine's own append order. There is no total order")
	fmt.Fprintln(stdout, "across machines: nothing here proves machine A's entry happened before machine B's.")
	return readErr
}

// fieldsOf pulls the flat string fields out of a record's data object. It reads
// the raw line rather than a typed struct so that a record written by a newer
// build, carrying fields this one does not know, is still readable.
func fieldsOf(raw string) map[string]string {
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return out
	}
	for k, v := range envelope.Data {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		out[k] = strings.Trim(string(v), `"`)
	}
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
