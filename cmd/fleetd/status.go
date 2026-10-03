package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

// A sync that fails leaves no trace in the journal, and one started by a hook
// fails where nobody is looking. So every sync notes its outcome in the clone's
// git directory, which is never committed, and `where` reports it: a machine
// whose syncs keep failing then says so, rather than looking idle.

// syncStatusFile is where the outcome is noted, inside the clone's .git directory.
const syncStatusFile = "fleetd-sync.json"

type syncStatus struct {
	LastAttempt *syncOutcome `json:"last_attempt,omitempty"`
	LastSuccess *syncOutcome `json:"last_success,omitempty"`
}

type syncOutcome struct {
	At string `json:"at"`
	// Started is when the sync began, before it read anything. A record
	// written after it may have missed the sync, however it ended.
	Started   string `json:"started,omitempty"`
	Error     string `json:"error,omitempty"`
	Published int    `json:"published,omitempty"`
	Received  int    `json:"received,omitempty"`
	Head      string `json:"head,omitempty"`
}

// noteSync records a sync's outcome for `where`. A journal directory that is
// not a clone has nowhere to note it; a failure to note it is only a warning,
// because the sync itself already did, or did not, do its work.
func noteSync(dir string, res gitsync.Result, syncErr error, started time.Time, stderr io.Writer) {
	gitDir, ok := gitsync.GitDir(dir)
	if !ok {
		return
	}
	path := filepath.Join(gitDir, syncStatusFile)
	st, _ := readSyncStatus(dir)
	now := syncOutcome{At: time.Now().UTC().Format(time.RFC3339), Started: started.UTC().Format(time.RFC3339Nano)}
	if syncErr != nil {
		now.Error = syncErr.Error()
	} else {
		now.Published, now.Received, now.Head = res.Published, res.Received, res.Head
		success := now
		st.LastSuccess = &success
	}
	st.LastAttempt = &now
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		err = gitsync.WriteNote(context.Background(), path, append(data, '\n'))
	}
	if err != nil {
		fmt.Fprintf(stderr, "fleetd: warning: could not note the sync's outcome in %s: %v\n", path, err)
	}
}

func readSyncStatus(dir string) (syncStatus, bool) {
	var st syncStatus
	gitDir, ok := gitsync.GitDir(dir)
	if !ok {
		return st, false
	}
	data, err := readNote(filepath.Join(gitDir, syncStatusFile))
	if err != nil || json.Unmarshal(data, &st) != nil {
		return syncStatus{}, false
	}
	return st, true
}

// publication is what the clone's copy of the remote says about one host file.
type publication struct {
	// Known is set when git could say; the other fields are empty otherwise.
	Known bool
	// At is the timestamp of the newest complete record in the remote's copy of
	// the file, as of this clone's last fetch; "" when the remote has no file.
	At string
	// Unpublished counts the file's complete records here that the remote lacks.
	Unpublished int
}

// publications reads, for each host file stem, what the remote's copy of it holds
// and how many of its records here it lacks. ok is false when the journal is not
// the top of a clone with an upstream, so there is nothing to compare. It reads
// the remote's tree once and each file's blob, never the history, so it costs the
// same however long the journal has been running. A host git could not answer for
// is left unknown rather than reported as unpublished.
func publications(dir string, stems []string) (map[string]publication, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	top, err := gitsync.Git(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil || !gitsync.SameDir(strings.TrimRight(top, "\r\n"), dir) {
		return nil, false
	}
	// The journal is on the branch init recorded, whatever branch the clone was
	// switched to since; a clone set up before init recorded one follows it.
	want := "@{upstream}"
	if gitDir, ok := gitsync.GitDir(dir); ok {
		if recorded, err := gitsync.RecordedBranch(gitDir); err == nil && recorded != "" {
			want = "refs/remotes/origin/" + recorded
		}
	}
	up, err := gitsync.Git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", want)
	if up = strings.TrimSpace(up); err != nil || up == "" {
		return nil, false
	}
	out := map[string]publication{}
	listing, err := gitsync.Git(ctx, dir, nil, "ls-tree", "-z", up)
	if err != nil {
		return out, true
	}
	blobs := map[string]string{}
	for _, entry := range strings.Split(listing, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); ok && len(fields) == 3 && fields[1] == "blob" {
			blobs[name] = fields[2]
		}
	}
	for _, stem := range stems {
		name := stem + ".jsonl"
		var remote []byte
		if oid, ok := blobs[name]; ok {
			blob, err := gitsync.Git(ctx, dir, nil, "cat-file", "blob", oid)
			if err != nil {
				continue
			}
			remote = []byte(blob)
		}
		p := publication{Known: true, At: newestTS(remote)}
		// A regular file only: a push can make a host file a link.
		if local, err := journal.ReadRegular(filepath.Join(dir, name)); err == nil {
			// Only complete records: one still being written waits for the next sync.
			local = bytes.ReplaceAll(local[:bytes.LastIndexByte(local, '\n')+1], []byte("\r\n"), []byte("\n"))
			p.Unpublished = unpublished(local, remote)
		}
		out[stem] = p
	}
	return out, true
}

// unpublished counts the records in local that remote lacks. For this machine's
// own file the remote's copy is a prefix of the local one, and the records after
// it are counted, repeats included; otherwise each line of remote accounts for
// one equal line of local.
func unpublished(local, remote []byte) int {
	if bytes.HasPrefix(local, remote) {
		return len(lines(local[len(remote):]))
	}
	have := map[string]int{}
	for _, l := range lines(remote) {
		have[l]++
	}
	n := 0
	for _, l := range lines(local) {
		if have[l] > 0 {
			have[l]--
			continue
		}
		n++
	}
	return n
}

// newestTS is the ts of the last record in journal content that has one.
func newestTS(content []byte) string {
	ls := lines(content[:bytes.LastIndexByte(content, '\n')+1])
	// A record re-filed from an identity this machine wrote under earlier is
	// appended late, with the time it was written, so it says nothing about how
	// recent the machine's newest record is, as `where` itself reads it.
	refiledOnly := ""
	for i := len(ls) - 1; i >= 0; i-- {
		var rec struct {
			TS   string                     `json:"ts"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(ls[i]), &rec) != nil || rec.TS == "" {
			continue
		}
		if _, refiled := rec.Data[refiledFrom]; !refiled {
			return rec.TS
		}
		refiledOnly = cmp.Or(refiledOnly, rec.TS)
	}
	return refiledOnly
}

// ago says how long ago an RFC3339 instant was, roughly, for a person.
func ago(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return ts + " (in the future by this machine's clock)"
	case d < time.Minute:
		return ts + " (just now)"
	case d < time.Hour:
		return fmt.Sprintf("%s (%d min ago)", ts, int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%s (%d h ago)", ts, int(d.Hours()))
	}
	return fmt.Sprintf("%s (%d days ago)", ts, int(d.Hours()/24))
}

// hookProblemAge is how long `where` keeps mentioning a problem a hook logged.
const hookProblemAge = 7 * 24 * time.Hour

// hookProblem returns the hook log beside the journal directory and its last
// line, when a hook logged a problem there within hookProblemAge. A hook's own
// output goes nowhere a person looks, so this is where its failures surface.
func hookProblem(journalDir string) (path, last string, ok bool) {
	path = filepath.Join(filepath.Dir(journalDir), hookLogName)
	f, err := journal.OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return path, "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || time.Since(info.ModTime()) > hookProblemAge {
		return path, "", false
	}
	offset := info.Size() - 4096
	if offset < 0 {
		offset = 0
	}
	tail := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(tail, offset); err != nil && !errors.Is(err, io.EOF) {
		return path, "", false
	}
	lines := lines(tail)
	if len(lines) == 0 {
		return path, "", false
	}
	return path, lines[len(lines)-1], true
}
