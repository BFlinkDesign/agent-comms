package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/cell"
	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

// A machine that records before it knows the fleet's salt files its records
// under another identity: a hook that fired before `fleetd init`, or one started
// with a FLEET_SALT the journal does not use. Nothing would ever publish those,
// so every sync files them under the machine's fleet identity first. Only files
// git does not track are taken: one it tracks was published under its identity
// and stays as it is.

// refileSettle is how long the journal's fleetd.json must have been in place
// before records are re-filed, so that a hook which resolved its salt just
// before it appeared has finished appending.
const refileSettle = 2 * time.Second

// preInitDir is where, inside the clone's git directory, the original of each
// re-filed file is kept. It is never committed and never read.
const preInitDir = "fleetd-pre-init"

// otherIdentities lists this machine under each salt it may have recorded with
// before it knew the fleet's: none, and FLEET_SALT.
func otherIdentities(me hostOut) []hostOut {
	var out []hostOut
	for _, salt := range []string{"", os.Getenv("FLEET_SALT")} {
		h := identity(salt)
		if h.ID == me.ID || slicesContainsID(out, h.ID) {
			continue
		}
		out = append(out, h)
	}
	return out
}

func slicesContainsID(hs []hostOut, id string) bool {
	for _, h := range hs {
		if h.ID == id {
			return true
		}
	}
	return false
}

// refilePending reports whether dir holds a file refile would take.
func refilePending(dir string, me hostOut) bool {
	for _, o := range otherIdentities(me) {
		if _, err := os.Lstat(filepath.Join(dir, journal.FileName(o.ID)+".jsonl")); err == nil {
			return true
		}
	}
	return false
}

// refile files under me the records this machine wrote under another identity,
// and moves each original into the clone's git directory. It does nothing until
// dir holds the journal's fleetd.json, me is the identity it gives, and it has
// been in place for refileSettle. It returns how many records it appended.
func refile(dir string, me hostOut, warn io.Writer) (int, error) {
	fleet, ok, err := gitsync.ReadFleet(dir)
	if err != nil || !ok || identity(fleet.Salt).ID != me.ID {
		return 0, nil
	}
	info, err := os.Stat(filepath.Join(dir, gitsync.FleetFile))
	if err != nil || time.Since(info.ModTime()) < refileSettle {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gitDir, err := gitsync.Git(ctx, dir, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return 0, nil
	}
	gitDir = strings.TrimSpace(gitDir)
	total := 0
	for _, o := range otherIdentities(me) {
		name := journal.FileName(o.ID) + ".jsonl"
		path := filepath.Join(dir, name)
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if _, err := gitsync.Git(ctx, dir, nil, "ls-files", "--error-unmatch", "--", name); err == nil {
			continue // tracked: published under that identity
		}
		n, skipped, err := refileOne(dir, path, o, me)
		total += n
		if err != nil {
			return total, err
		}
		keep := filepath.Join(gitDir, preInitDir)
		if err := os.MkdirAll(keep, 0o700); err != nil {
			return total, err
		}
		dest := filepath.Join(keep, fmt.Sprintf("%s.%d", name, time.Now().UnixNano()))
		if err := gitsync.RenameRetry(path, dest); err != nil {
			return total, err
		}
		if skipped > 0 {
			fmt.Fprintf(warn, "fleetd: warning: %s of %s could not be filed under this machine's fleet identity; "+
				"the file is kept as %s\n", plural(skipped, "record"), name, dest)
		}
	}
	return total, nil
}

// refileOne appends to me's file every complete record in path that it does not
// hold yet, re-encoded under me. A line that is not a record of identity o is
// skipped and counted.
func refileOne(dir, path string, o, me hostOut) (added, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	store, err := journal.Open(dir)
	if err != nil {
		return 0, 0, err
	}
	have := map[string]bool{}
	recs, _ := store.Read(me.ID)
	for _, r := range recs {
		have[r.ID] = true
	}
	// A final line without a newline may still be being written; it stays in the
	// kept original, and is counted as skipped.
	complete := data[:bytes.LastIndexByte(data, '\n')+1]
	if len(bytes.TrimSpace(data[len(complete):])) > 0 {
		skipped++
	}
	for _, line := range lines(complete) {
		c, err := reencode(line, o.ID, me.ID)
		if err != nil {
			skipped++
			continue
		}
		if have[c.ID] {
			continue
		}
		if err := store.Append(me.ID, c); err != nil {
			return added, skipped, err
		}
		have[c.ID] = true
		added++
	}
	return added, skipped, nil
}

// reencode rebuilds a record of host id from as one of host id to: the same
// record with data's host.id replaced, and so a new content-addressed id. Only
// values a journal record holds are accepted: strings, booleans and integers.
func reencode(line, from, to string) (cell.Cell, error) {
	var rec struct {
		Type    string                     `json:"type"`
		From    string                     `json:"from"`
		TS      string                     `json:"ts"`
		Channel string                     `json:"channel"`
		Data    map[string]json.RawMessage `json:"data"`
		Refs    []string                   `json:"refs"`
		TTL     int                        `json:"ttl"`
		Tags    []string                   `json:"tags"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return cell.Cell{}, err
	}
	var hostID string
	if err := json.Unmarshal(rec.Data["host.id"], &hostID); err != nil || hostID != from {
		return cell.Cell{}, errors.New("not a record of that identity")
	}
	data := make(map[string]cell.Value, len(rec.Data))
	for k, raw := range rec.Data {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return cell.Cell{}, err
		}
		switch x := v.(type) {
		case string:
			data[k] = cell.S(x)
		case bool:
			data[k] = cell.B(x)
		case json.Number:
			n, err := x.Int64()
			if err != nil {
				return cell.Cell{}, fmt.Errorf("data %q is not an integer", k)
			}
			data[k] = cell.I(n)
		default:
			return cell.Cell{}, fmt.Errorf("data %q is not a string, boolean or integer", k)
		}
	}
	data["host.id"] = cell.S(to)
	return cell.New(rec.Type, rec.From, rec.TS, rec.Channel, data, rec.Refs, rec.Tags, rec.TTL)
}
