package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
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
	At        string `json:"at"`
	Error     string `json:"error,omitempty"`
	Published int    `json:"published,omitempty"`
	Received  int    `json:"received,omitempty"`
	Head      string `json:"head,omitempty"`
}

// noteSync records a sync's outcome for `where`. A journal directory that is
// not a clone has nowhere to note it; a failure to note it is only a warning,
// because the sync itself already did, or did not, do its work.
func noteSync(dir string, res gitsync.Result, syncErr error, stderr io.Writer) {
	gitDir := filepath.Join(dir, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return
	}
	path := filepath.Join(gitDir, syncStatusFile)
	st, _ := readSyncStatus(dir)
	now := syncOutcome{At: time.Now().UTC().Format(time.RFC3339)}
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
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, append(data, '\n'), 0o644); err == nil {
			err = renameRetry(tmp, path)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "fleetd: warning: could not note the sync's outcome in %s: %v\n", path, err)
	}
}

func readSyncStatus(dir string) (syncStatus, bool) {
	var st syncStatus
	data, err := os.ReadFile(filepath.Join(dir, ".git", syncStatusFile))
	if err != nil || json.Unmarshal(data, &st) != nil {
		return syncStatus{}, false
	}
	return st, true
}

// publication is what the clone's copy of the remote says about one host file.
type publication struct {
	// At is when the remote branch, as of this clone's last fetch, last changed
	// the file, by the publishing machine's clock; "" if it never had it.
	At string
	// Unpublished counts the file's complete records here that the remote lacks.
	Unpublished int
}

// publications reads, for each host file stem, when the remote last published it
// and how many of its records on this machine it still lacks. ok is false when
// the journal is not a clone with an upstream, so there is nothing to compare.
// Records here that the remote lacks are this machine's unpublished work, or
// the records of a second identity this machine was given, which only a sync
// with that identity's salt would publish.
func publications(dir string, stems []string) (map[string]publication, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	up, err := gitsync.Git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "@{upstream}")
	if up = strings.TrimSpace(up); err != nil || up == "" {
		return nil, false
	}
	out := map[string]publication{}
	for _, stem := range stems {
		name := stem + ".jsonl"
		var p publication
		if at, err := gitsync.Git(ctx, dir, nil, "log", "-1", "--format=%cI", up, "--", name); err == nil {
			p.At = strings.TrimSpace(at)
		}
		published := map[string]bool{}
		if p.At != "" {
			if blob, err := gitsync.Git(ctx, dir, nil, "cat-file", "blob", up+":"+name); err == nil {
				for _, line := range lines([]byte(blob)) {
					published[line] = true
				}
			}
		}
		if local, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			// Only complete records: one still being written waits for the next sync.
			local = local[:strings.LastIndexByte(string(local), '\n')+1]
			for _, line := range lines(local) {
				if !published[line] {
					p.Unpublished++
				}
			}
		}
		out[stem] = p
	}
	return out, true
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
