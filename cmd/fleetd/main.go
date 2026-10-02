// Command fleetd records and answers what happened on which machine.
//
// It exists because git cannot answer that. A commit carries an author, a
// committer and a timezone offset; none of them identify a host. Measuring this
// fleet's own history found ten distinct commit identities for one person and no
// signal separating one workstation from another, so "which PC did I do that on"
// was never answerable after the fact. fleetd records it at the time.
//
// Five commands, deliberately, plus version:
//
//	fleetd init URL        set up this machine's journal, once
//	fleetd host            what this machine is, and how confident that is
//	fleetd record ...      append one host-attributed record
//	fleetd sync            publish this machine's records, receive the others'
//	fleetd where           per machine, what it was last doing, and how fresh that is
//	fleetd hook TOOL       record a Claude Code, Cursor, Codex or Grok event
//	fleetd version         which build this is
//
// Every command takes --json, so the same surface serves a person at a terminal
// and a program reading structured output. That is what a later MCP server would
// wrap; there is no second implementation to keep in sync.
//
// The journal directory is resolved from --dir, else $COMMS_CHANNELS/journal,
// else ~/.ai/channels/journal, where the rest of this bus keeps its channels.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/cell"
	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/hostid"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

const usage = `fleetd — record and answer what happened on which machine

usage:
  fleetd init   [--json] [--dir D] [--salt S] [--reclaim] [--timeout 2m] URL
  fleetd host   [--json] [--dir D] [--salt S]
  fleetd record [--json] [--dir D] [--salt S] --type T [--note N] [--repo R] [--branch B] [--agent A] [--include-user]
  fleetd sync   [--json] [--dir D] [--salt S] [--timeout 60s]
  fleetd where  [--json] [--dir D] [--limit N]
  fleetd hook   <claude|cursor|codex|grok> [--dir D] [--salt S] [--timeout 40s] [--no-sync] [--json] [event-json]
  fleetd version

The journal directory is --dir, else $COMMS_CHANNELS/journal, else ~/.ai/channels/journal.
The where command reports an error, rather than "no records", when that
directory does not exist -- usually the sign of a mistyped path.

init makes the journal directory a clone of the journal repository URL, once per
machine, without moving or deleting anything in it, and then syncs. An empty
repository gets its first commit, holding fleetd.json with a salt for the fleet;
one that already holds records needs the salt its machines use, from --salt or
$FLEET_SALT. When this machine's journal directory was set up again, or restored
from an older copy, its file no longer starts with the records the journal holds
for it, and nothing is published until init --reclaim puts those records back in
front of the newer ones.

The salt is the one in the journal's fleetd.json, so every machine that clones the
journal uses the same one. --salt may repeat it but not contradict it; $FLEET_SALT
that contradicts it is overridden, with a warning. Without fleetd.json they supply
it. It separates this fleet's host digests from any other and must be the same on
every machine, or one machine will appear as several. It is not a credential.
Records this machine wrote under another salt before it knew the fleet's are
filed under the fleet's by the next sync.

sync needs the journal directory to be the root of a clone of the journal
repository, used for nothing else. It publishes only this machine's file, as of
its last complete record, in a commit built on top of the remote; it never
rebases and never rewrites this machine's file, so a record written during a sync
is never lost. Each machine writes only its own file, so machines never
conflict. A file with changes the remote does not have is never overwritten;
sync names it instead.

The OS account name is recorded only with --include-user. These records are
meant to be committed, and on a domain-joined host that name carries the domain
with it.

hook is what an AI tool's own hook configuration runs; AGENTS.md has a ready
configuration for claude, cursor, codex and grok. It reads the event the way the
tool documents it (JSON on stdin, or for codex's notify program the last
argument) and appends one record through the same path as record: --agent is
the tool, --type is session, turn or hook, and --repo and --branch come from the
git repository at the event's working directory. A turn is recorded at most once
every 30 minutes per session. A session end then syncs, bounded by --timeout,
unless --no-sync is given; so does a turn codex's notify program records, since
codex neither waits for nor limits it. A turn from a tool's own hooks does not
sync, since the person is waiting. A hook never disturbs the tool that ran it: it
prints nothing (--json is for trying it by hand), always exits 0, and appends any
problem to fleetd-hook.log beside the journal directory.
`

// version is set by the release build (-ldflags "-X main.version=fleetd-v1.2.3").
// Any other build reports "dev" and, when Go recorded it, the commit it was
// built from, so two machines can always tell whether they run the same fleetd.
var version = "dev"

func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	switch {
	case rev == "":
		return version
	case dirty:
		return version + "-" + rev + "-modified"
	default:
		return version + "-" + rev
	}
}

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
	case "init":
		return cmdInit(args[1:], stdout, stderr)
	case "host":
		return cmdHost(args[1:], stdout, stderr)
	case "record":
		return cmdRecord(args[1:], stdout, stderr)
	case "sync":
		return cmdSync(args[1:], stdout, stderr)
	case "where":
		return cmdWhere(args[1:], stdout, stderr)
	case "hook":
		return cmdHook(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "fleetd", buildVersion(), runtime.GOOS+"/"+runtime.GOARCH)
		return nil
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// resolveSalt finds the fleet salt. The journal's fleetd.json decides, since every
// machine must derive host ids the same way: --salt may repeat it but not
// contradict it, which is an error. $FLEET_SALT that contradicts it is overridden
// with a warning instead: refusing would drop every record of a hook started with
// a stale one. A journal without fleetd.json uses --salt, else $FLEET_SALT.
//
// A fleetd.json that is not valid is not trusted. The salt it last held, kept in
// the clone's git directory, is used instead, with a warning, so that a sync can
// still bring in a fixed file. An empty salt still produces stable digests; it
// just does not separate this fleet from another using the same scheme, so the
// caller is told rather than silently given a weaker identity.
func resolveSalt(flagValue, journalDir string, stderr io.Writer) (string, error) {
	fleet, ok, err := gitsync.ReadFleet(journalDir)
	if err != nil {
		if !errors.Is(err, gitsync.ErrBadFleetFile) {
			return "", err
		}
		if cached := cachedSalt(journalDir); cached != "" {
			fmt.Fprintf(stderr, "fleetd: warning: %v; the salt it last held is used until a sync brings in a fixed one\n", err)
			fleet, ok = gitsync.Fleet{Salt: cached}, true
		} else {
			fmt.Fprintf(stderr, "fleetd: warning: %v; it is ignored until a sync brings in a fixed one\n", err)
		}
	}
	env := os.Getenv("FLEET_SALT")
	switch {
	case ok && flagValue != "" && flagValue != fleet.Salt:
		return "", saltMismatch("--salt", journalDir)
	case ok:
		if flagValue == "" && env != "" && env != fleet.Salt {
			fmt.Fprintf(stderr, "fleetd: warning: FLEET_SALT differs from the salt in %s's %s, which is used; unset FLEET_SALT\n",
				journalDir, gitsync.FleetFile)
		}
		// A changed salt leaves this machine's records under the old one
		// unpublished: noting it lets a sync file them under the new one.
		if prev := cachedSalt(journalDir); prev != "" && prev != fleet.Salt {
			_ = gitsync.NotePastSalt(filepath.Join(journalDir, ".git"), prev)
		}
		cacheSalt(journalDir, fleet.Salt)
		return fleet.Salt, nil
	case flagValue != "":
		return flagValue, nil
	case env != "":
		return env, nil
	}
	fmt.Fprintln(stderr, "fleetd: warning: no fleetd.json in the journal, no --salt and no FLEET_SALT; host digests are unseparated")
	return "", nil
}

// saltCacheName is the file, in a clone's .git directory, that keeps the last
// valid salt fleetd.json held.
const saltCacheName = "fleetd-salt"

func cachedSalt(journalDir string) string {
	data, err := os.ReadFile(filepath.Join(journalDir, ".git", saltCacheName))
	if err != nil {
		return ""
	}
	// Only the newline cacheSalt ends it with: a salt is used exactly as given,
	// spaces and all, or the same machine gets a second id.
	return strings.TrimSuffix(string(data), "\n")
}

// cacheSalt keeps salt for cachedSalt, in a clone only, and only when it changed.
// It is best effort: without it, an invalid fleetd.json is merely ignored.
func cacheSalt(journalDir, salt string) {
	gitDir := filepath.Join(journalDir, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() || cachedSalt(journalDir) == salt {
		return
	}
	path := filepath.Join(gitDir, saltCacheName)
	if err := os.WriteFile(path+".tmp", []byte(salt+"\n"), 0o600); err == nil {
		_ = gitsync.RenameRetry(path+".tmp", path)
	}
}

// resolveDir finds the journal directory: --dir, else $COMMS_CHANNELS/journal,
// else .ai/channels/journal under the home directory, where the rest of this bus
// keeps its channels. Never the current directory: a hook runs fleetd from
// whatever project it fires in, and a journal written there is never synced.
func resolveDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("COMMS_CHANNELS"); v != "" {
		return filepath.Join(v, "journal"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no --dir, no COMMS_CHANNELS and no home directory to find the journal in: %w", err)
	}
	return filepath.Join(home, ".ai", "channels", "journal"), nil
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
	dir := fs.String("dir", "", "journal directory, whose fleetd.json holds the salt")
	salt := fs.String("salt", "", "fleet salt (else the journal's fleetd.json, else $FLEET_SALT)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	journalDir, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	fleetSalt, err := resolveSalt(*salt, journalDir, stderr)
	if err != nil {
		return err
	}

	h := identity(fleetSalt)
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
	var req recordRequest
	fs.StringVar(&req.dir, "dir", "", "journal directory (else $COMMS_CHANNELS/journal, else ~/.ai/channels/journal)")
	fs.StringVar(&req.salt, "salt", "", "fleet salt (else the journal's fleetd.json, else $FLEET_SALT)")
	fs.StringVar(&req.typ, "type", "", "record type, e.g. observation, handoff, note (required)")
	fs.StringVar(&req.note, "note", "", "what happened, in your own words")
	fs.StringVar(&req.repo, "repo", "", "repository the work was in")
	fs.StringVar(&req.branch, "branch", "", "branch the work was on")
	fs.StringVar(&req.agent, "agent", "", "which tool produced this, as name/role")
	fs.StringVar(&req.at, "at", "", "RFC3339 timestamp (default: now)")
	fs.BoolVar(&req.includeUser, "include-user", false,
		"publish the OS account name in clear; off by default because these records are committed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rec, err := appendRecord(req, stderr)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, map[string]any{"id": rec.id, "host": rec.host.ID, "file": rec.file})
	}
	fmt.Fprintf(stdout, "%s  recorded on %s\n", rec.id, rec.host.Name)
	return nil
}

// recordRequest is one record to append. `fleetd record` fills it from its flags
// and `fleetd hook` from a tool's event, and both go through appendRecord, so a
// record a hook wrote has exactly the shape and host attribution of one a person
// wrote.
type recordRequest struct {
	dir, salt, typ, note, repo, branch, agent, at string
	// extra holds further data fields: the tool and session a hook recorded.
	extra       map[string]string
	includeUser bool
}

// recorded says what appendRecord wrote and where.
type recorded struct {
	id   string
	host hostOut
	file string
}

// appendRecord builds one host-attributed cell and appends it to this host's
// journal file. Warnings, such as a missing salt, go to warn.
func appendRecord(req recordRequest, warn io.Writer) (recorded, error) {
	if strings.TrimSpace(req.typ) == "" {
		return recorded{}, errors.New("--type is required")
	}
	journalDir, err := resolveDir(req.dir)
	if err != nil {
		return recorded{}, err
	}
	fleetSalt, err := resolveSalt(req.salt, journalDir, warn)
	if err != nil {
		return recorded{}, err
	}

	h := identity(fleetSalt)
	from := req.agent
	if strings.TrimSpace(from) == "" {
		from = "fleetd/" + h.Name
	}

	ts := req.at
	if ts == "" {
		// Nanosecond precision, not whole seconds. The cell id is derived from
		// the content including this timestamp, so at second resolution two
		// genuinely distinct records written in the same second produce
		// byte-identical cells with the same id -- and since a reader is
		// documented to collapse colliding ids, one of the two events would
		// simply disappear while `where` still counted both. The Python writer
		// uses isoformat(), which carries microseconds, and does not have this
		// problem; fleetd introduced it.
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		return recorded{}, fmt.Errorf("--at %q is not an RFC3339 timestamp: %w", ts, err)
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
	for k, v := range req.extra {
		if strings.TrimSpace(v) != "" {
			data[k] = cell.S(v)
		}
	}
	for k, v := range map[string]string{"note": req.note, "repo": req.repo, "branch": req.branch} {
		if strings.TrimSpace(v) != "" {
			data[k] = cell.S(v)
		}
	}
	// The OS account is opt-in. internal/hostid goes to some length not to
	// publish the machine's hardware identifier, and publishing the account name
	// in the same record would undo that: on a domain-joined Windows host
	// user.Current().Username is of the form DOMAIN\account, so the default
	// behaviour would have committed the AD domain and the operator's account
	// name into a repository on every record.
	if req.includeUser && strings.TrimSpace(h.User) != "" {
		data["user"] = cell.S(h.User)
	}

	c, err := cell.New(req.typ, from, ts, "journal", data, nil, nil, 0)
	if err != nil {
		return recorded{}, err
	}

	store, err := journal.Open(journalDir)
	if err != nil {
		return recorded{}, err
	}
	if err := store.Append(h.ID, c); err != nil {
		return recorded{}, err
	}
	return recorded{id: c.ID, host: h, file: filepath.Join(store.Dir(), journal.FileName(h.ID)+".jsonl")}, nil
}

func cmdSync(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	dir := fs.String("dir", "", "journal directory (else $COMMS_CHANNELS/journal, else ~/.ai/channels/journal)")
	salt := fs.String("salt", "", "fleet salt (else the journal's fleetd.json, else $FLEET_SALT)")
	timeout := fs.Duration("timeout", 60*time.Second, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	journalDir, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	fleetSalt, err := resolveSalt(*salt, journalDir, stderr)
	if err != nil {
		// A sync that cannot start has still failed, and `where` says so.
		noteSync(journalDir, gitsync.Result{}, err, time.Now(), stderr)
		return err
	}
	h := identity(fleetSalt)
	res, storeDir, err := syncJournal(journalDir, h, *timeout, false, stderr)
	if err != nil {
		// Lock files removed before the sync failed are still worth knowing
		// about: the next sync would otherwise not mention them at all.
		if len(res.Cleared) > 0 {
			fmt.Fprintf(stderr, "fleetd: removed %s older than ten minutes from the clone: %s\n",
				plural(len(res.Cleared), "git lock file"), strings.Join(res.Cleared, ", "))
		}
		return err
	}
	if *asJSON {
		return writeJSON(stdout, res)
	}
	fmt.Fprintf(stdout, "published %s from %s; received %s from other machines; journal at %.7s\n",
		plural(res.Published, "record"), h.Name, plural(res.Received, "commit"), res.Head)
	if len(res.Kept) > 0 {
		fmt.Fprintf(stdout, "kept this machine's copy of %s: it has changes the remote does not have.\n"+
			"  To take the remote's copy:  git -C %s checkout '@{upstream}' -- <file>\n",
			strings.Join(res.Kept, ", "), storeDir)
	}
	if len(res.Cleared) > 0 {
		fmt.Fprintf(stdout, "removed %s older than ten minutes from the clone: %s\n",
			plural(len(res.Cleared), "git lock file"), strings.Join(res.Cleared, ", "))
	}
	return nil
}

// syncJournal publishes host h's journal file in journalDir, brings in every
// other host's, gives up after timeout, and notes the outcome for `where`.
// `fleetd sync`, `fleetd hook` and `fleetd init` all sync through it. Holding
// the sync's lock, it first files under h the records this machine wrote under
// another identity (see refile), and with reclaim, which only `fleetd init
// --reclaim` sets, puts h's published records back at the start of its own file
// (see reconcileOwn).
func syncJournal(journalDir string, h hostOut, timeout time.Duration, reclaim bool, stderr io.Writer) (gitsync.Result, string, error) {
	store, err := journal.Open(journalDir)
	if err != nil {
		return gitsync.Result{}, "", err
	}
	prepare := func(gitDir string) {
		if reclaim {
			if err := reconcileOwn(store.Dir(), gitDir, h); err != nil {
				fmt.Fprintf(stderr, "fleetd: warning: putting this machine's published records back in its journal file: %v\n", err)
			}
		}
		if _, err := refile(store.Dir(), gitDir, h, stderr); err != nil {
			fmt.Fprintf(stderr, "fleetd: warning: filing this machine's earlier records under its fleet identity: %v\n", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	res, err := gitsync.Sync(ctx, gitsync.Options{
		Dir:     store.Dir(),
		File:    filepath.Join(store.Dir(), journal.FileName(h.ID)+".jsonl"),
		Message: fmt.Sprintf("journal: %s (%s)", h.Name, h.ID),
		Prepare: prepare,
	})
	noteSync(store.Dir(), res, err, started, stderr)
	return res, store.Dir(), err
}

// printSyncStatus says how fresh the answer is: a clone holds the other machines'
// records as of this machine's last successful sync.
func printSyncStatus(stdout io.Writer, dir string) {
	if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		return
	}
	switch st, ok := readSyncStatus(dir); {
	case !ok:
		fmt.Fprintln(stdout, "this machine has not synced this journal yet; the other machines' records may be missing")
	case st.LastAttempt != nil && st.LastAttempt.Error != "":
		fmt.Fprintf(stdout, "this machine's last sync, %s, FAILED: %s\n", ago(st.LastAttempt.At), st.LastAttempt.Error)
		if st.LastSuccess != nil {
			fmt.Fprintf(stdout, "the records below are as of its last successful sync, %s\n", ago(st.LastSuccess.At))
		}
	case st.LastSuccess != nil:
		fmt.Fprintf(stdout, "the records below are as of this machine's last sync, %s\n", ago(st.LastSuccess.At))
	}
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
	// LastPublished is the timestamp of the newest record the remote has for this
	// host, as of this clone's last sync. Absent when the remote has no file for
	// it, when the journal is not a clone, or when git could not say.
	LastPublished string `json:"last_published,omitempty"`
	// Unpublished counts this host's records here that the remote lacks: this
	// machine's work not yet synced. Absent when it is none, or unknown.
	Unpublished int `json:"unpublished,omitempty"`
	// Recent is the entries before the last one, newest first, bounded by
	// --limit. It is emitted in JSON as well as to a terminal, so a program and a
	// person see the same history.
	Recent []recentEntry `json:"recent,omitempty"`
	// TimestampsOutOfOrder is set when this host's records were appended in an
	// order its own timestamps disagree with. Append order is what is reported,
	// because it is what actually happened and does not depend on a clock; when
	// the two disagree that is worth saying rather than quietly presenting an
	// older entry as the newest.
	TimestampsOutOfOrder bool `json:"timestamps_out_of_order,omitempty"`

	// lastAt is LastTS parsed to an instant, used for ordering. Unexported so it
	// does not appear in the JSON: the timestamp is already there as last_ts, and
	// a second rendering of the same fact could only disagree with it.
	lastAt time.Time
	// publicationKnown is set when git said what the remote has for this host.
	publicationKnown bool
}

type recentEntry struct {
	TS   string `json:"ts"`
	Type string `json:"type,omitempty"`
	Repo string `json:"repo,omitempty"`
	Note string `json:"note,omitempty"`
}

func cmdWhere(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("where", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	dir := fs.String("dir", "", "journal directory")
	limit := fs.Int("limit", 3,
		"total records to show per machine, counting the most recent one (so 3 = last plus 2 before it)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *limit < 1 {
		return fmt.Errorf("--limit must be at least 1, got %d", *limit)
	}

	journalDir, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	store, err := journal.Open(journalDir)
	if err != nil {
		return err
	}
	records, readErr := store.ReadAll()
	// A hook's problems and a failed sync go nowhere a person looks, so they
	// surface here, before anything else: most of all when there is nothing else
	// to show.
	if !*asJSON {
		if path, last, ok := hookProblem(store.Dir()); ok {
			fmt.Fprintf(stdout, "a hook logged a problem in %s; the last one:\n  %s\n", path, last)
		}
		printSyncStatus(stdout, store.Dir())
	}
	// A store that does not exist is a different answer from a store with nothing
	// in it, and the difference matters: the first usually means a mistyped path.
	if errors.Is(readErr, journal.ErrNoStore) {
		return readErr
	}
	// A damaged journal is reported and the intact records are still used. Refusing
	// to answer at all because one machine's file was truncated would make a bad
	// day worse.
	if readErr != nil {
		fmt.Fprintln(stderr, "fleetd: warning:", readErr)
	}
	if len(records) == 0 {
		if *asJSON {
			// A program asked: an empty journal is an empty list, not a sentence.
			if err := writeJSON(stdout, []whereEntry{}); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(stdout, "no records in %s\n", store.Dir())
		}
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
	for host, all := range byHost {
		// Within a host the records already arrive in file order -- the order the
		// appends actually committed -- because readFile numbers them as it reads
		// and ReadAll keeps each host's slice contiguous. That is the ordering
		// primitive here, and unlike a timestamp it cannot be wrong because a
		// writer's clock was. A record re-filed from another identity this
		// machine wrote under earlier is the exception: it was appended late, so
		// its place says nothing about when it happened, and it is left out of
		// what was last.
		recs := all
		if live := slices.DeleteFunc(slices.Clone(all), func(r journal.Record) bool {
			return fieldsOf(r.Raw)[refiledFrom] != ""
		}); len(live) > 0 {
			recs = live
		}
		last := recs[len(recs)-1]
		f := fieldsOf(last.Raw)
		e := whereEntry{
			Host: host, HostName: f["host.name"], OS: f["host.os"],
			Stable: f["host.stable"] == "true", Records: len(all),
			LastTS: last.TS, LastType: last.Type, LastRepo: f["repo"], LastNote: f["note"],
			lastAt: parseTS(last.TS),
		}
		// --limit applies to both output forms. It used to be read only by the
		// human branch, so a program asking --json --limit 10 silently received
		// one entry and could not tell the flag had been ignored.
		for i := len(recs) - 2; i >= 0 && i > len(recs)-1-*limit; i-- {
			rf := fieldsOf(recs[i].Raw)
			e.Recent = append(e.Recent, recentEntry{
				TS: recs[i].TS, Type: recs[i].Type, Repo: rf["repo"], Note: rf["note"],
			})
		}
		for _, r := range recs[:len(recs)-1] {
			if parseTS(r.TS).After(e.lastAt) {
				e.TimestampsOutOfOrder = true
				break
			}
		}
		out = append(out, e)
	}
	stems := make([]string, 0, len(out))
	for _, e := range out {
		stems = append(stems, e.Host)
	}
	pubs, cloned := publications(store.Dir(), stems)
	unknown := 0
	for i := range out {
		p := pubs[out[i].Host]
		if cloned && !p.Known {
			unknown++
		}
		out[i].LastPublished, out[i].Unpublished, out[i].publicationKnown = p.At, p.Unpublished, p.Known
	}
	if unknown > 0 {
		fmt.Fprintf(stderr, "fleetd: warning: git could not say what the remote has for %s; their publication is left out\n",
			plural(unknown, "machine"))
	}
	// Compared as instants, not as strings. Lexical comparison of RFC3339 is only
	// correct when every timestamp is in UTC "Z" form, and it is not: --at accepts
	// an offset, and hive/cell.py builds every cell's ts with
	// datetime.now(UTC).astimezone().isoformat() -- local time with an offset --
	// so records from the existing Python bus sorted wrong by construction. A
	// machine could be presented as the most recent when it was in fact the
	// oldest, which is a wrong answer to the only question this tool asks.
	// SliceStable with a host tiebreak so equal instants do not reorder per run.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].lastAt.Equal(out[j].lastAt) {
			return out[i].lastAt.After(out[j].lastAt)
		}
		return out[i].Host < out[j].Host
	})

	if *asJSON {
		return writeJSON(stdout, out)
	}
	for _, e := range out {
		name := e.HostName
		if name == "" {
			name = e.Host
		}
		marker := ""
		if !e.Stable {
			marker = "  [attribution: hostname only]"
		}
		fmt.Fprintf(stdout, "\n%s (%s, %s)%s\n", name, e.OS, plural(e.Records, "record"), marker)
		fmt.Fprintf(stdout, "  last  %s  %s\n", e.LastTS, e.LastType)
		if e.TimestampsOutOfOrder {
			fmt.Fprintln(stdout, "        (appended last, but an earlier entry carries a later timestamp —")
			fmt.Fprintln(stdout, "         append order is reported, so this machine's clock or write order is suspect)")
		}
		if e.LastRepo != "" {
			fmt.Fprintf(stdout, "  repo  %s\n", e.LastRepo)
		}
		if e.LastNote != "" {
			fmt.Fprintf(stdout, "  note  %s\n", e.LastNote)
		}
		if e.publicationKnown {
			if e.LastPublished != "" {
				fmt.Fprintf(stdout, "  published up to  %s\n", ago(e.LastPublished))
			} else {
				fmt.Fprintln(stdout, "  published  none: the remote has no file for this machine")
			}
			if e.Unpublished > 0 {
				fmt.Fprintf(stdout, "  NOT PUBLISHED: %s here that the remote does not have\n", plural(e.Unpublished, "record"))
			}
		}
		if len(e.Recent) > 0 {
			fmt.Fprintln(stdout, "  before that:")
			for _, r := range e.Recent {
				fmt.Fprintf(stdout, "    %s  %-12s %s\n", r.TS, r.Type, r.Note)
			}
		}
	}
	fmt.Fprintln(stdout, "\nRecords are ordered by each machine's own append order. There is no total order")
	fmt.Fprintln(stdout, "across machines: nothing here proves machine A's entry happened before machine B's.")
	return readErr
}

// parseTS turns a record's timestamp into an instant for comparison. A record
// whose timestamp will not parse sorts as the zero time, i.e. last, rather than
// being dropped: a record that arrived is evidence even when its clock field is
// unusable, and hiding it would be the silent-guess failure again.
func parseTS(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
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
