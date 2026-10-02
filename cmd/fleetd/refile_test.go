package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

// remoteHolds reports whether any file on the remote's branch holds text.
func remoteHolds(t *testing.T, remote, branch, text string) bool {
	t.Helper()
	out, err := osexec.Command("git", "--git-dir", remote, "ls-tree", "--name-only", branch).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	for _, name := range strings.Fields(string(out)) {
		blob, _ := osexec.Command("git", "--git-dir", remote, "show", branch+":"+name).CombinedOutput()
		if strings.Contains(string(blob), text) {
			return true
		}
	}
	return false
}

// recordWithout records note in dir under the identity this machine had before
// it knew the journal's salt: fleetd.json is out of the way while it does.
func recordWithout(t *testing.T, dir string, args ...string) {
	t.Helper()
	aside := filepath.Join(t.TempDir(), gitsync.FleetFile)
	if err := os.Rename(filepath.Join(dir, gitsync.FleetFile), aside); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(aside, filepath.Join(dir, gitsync.FleetFile)); err != nil {
			t.Fatal(err)
		}
	}()
	if _, _, err := exec(t, append([]string{"record", "--dir", dir, "--type", "note"}, args...)...); err != nil {
		t.Fatal(err)
	}
}

func fleetFile(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, strings.ReplaceAll(hostID(t, "--dir", dir), ":", "-")+".jsonl")
}

// recordLine returns the line `fleetd record` writes for note under the identity
// this machine has without a salt, from a directory of its own, for a test to put
// where a process with that identity would have appended it.
func recordLine(t *testing.T, note string) string {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", note); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, strings.ReplaceAll(hostID(t, "--salt", "", "--dir", dir), ":", "-")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Two syncs that start together, as a hook's and init's do, must not both file
// the same earlier records.
func TestTwoSyncsAtOnceFileEarlierRecordsOnce(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		recordWithout(t, dir, "--note", "before the salt was known")
	}
	fleetFileAged(t, dir)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = exec(t, "sync", "--dir", dir) }()
	}
	wg.Wait()
	data, err := os.ReadFile(fleetFile(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, l := range lines(data) {
		var r struct{ ID string }
		_ = json.Unmarshal([]byte(l), &r)
		seen[r.ID] = true
	}
	if n := len(lines(data)); n != len(seen) || n != 30 {
		t.Fatalf("the fleet identity's file holds %d records and %d distinct ids, want 30 of each", n, len(seen))
	}
}

// A process that had the file open when a sync moved it appends to the moved
// copy. The next sync still files that record.
func TestARecordAppendedToAMovedCopyIsFiledByTheNextSync(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	recordWithout(t, dir, "--note", "first")
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	moved, _ := filepath.Glob(filepath.Join(dir, ".git", "fleetd-pre-init", "host-*.jsonl.*"))
	if len(moved) != 1 {
		t.Fatalf("moved copies: %v", moved)
	}
	// The same identity's record, written to the moved copy.
	scratch := filepath.Join(t.TempDir(), "scratch")
	if _, _, err := exec(t, "record", "--dir", scratch, "--salt", "", "--type", "note", "--note", "late"); err != nil {
		t.Fatal(err)
	}
	late, err := os.ReadFile(filepath.Join(scratch, filepath.Base(strings.TrimSuffix(moved[0], filepath.Ext(moved[0])))))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(moved[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(late); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "late"); !ok {
		t.Fatal("a record appended to the moved copy was never filed")
	}
	if n, _ := remoteHostRecords(t, remote, "s", "late"); n != 2 {
		t.Fatalf("the remote holds %d records for this machine, want first and late once each", n)
	}
}

// A record this machine wrote under another identity whose file git already
// tracks, and that was never synced, must still reach the remote.
func TestAnUnsyncedRecordOfATrackedOtherIdentityIsPublished(t *testing.T) {
	remote := emptyJournalRemote(t)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("journal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "start")
	gitIn(t, seed, "push", "--quiet", "-u", "origin", "main")
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "published under no salt"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "never synced before init"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		fleetFileAged(t, dir)
		if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := remoteHostRecords(t, remote, "s", "never synced before init"); !ok {
		t.Fatal("a record written under the old identity before init is on no remote file")
	}
	if n := remoteCount(t, remote, "published under no salt"); n != 1 {
		t.Fatalf("the record published under the old identity is on the remote %d times, want once", n)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after the syncs:\n%s", out)
	}
}

// remoteCount counts the lines holding text in every file on the remote's main.
func remoteCount(t *testing.T, remote, text string) int {
	t.Helper()
	out, err := osexec.Command("git", "--git-dir", remote, "ls-tree", "--name-only", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	n := 0
	for _, name := range strings.Fields(string(out)) {
		blob, _ := osexec.Command("git", "--git-dir", remote, "show", "main:"+name).CombinedOutput()
		n += strings.Count(string(blob), text)
	}
	return n
}

// A machine whose journal directory was lost runs init again. A hook that fired
// in between, with FLEET_SALT set to the fleet's salt, wrote this machine's file
// anew. Syncing must still work, and publish that record.
func TestInitAgainAfterTheJournalDirectoryWasLostStillPublishes(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "published first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "s")
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "recorded before init ran again"); err != nil {
		t.Fatal(err)
	}
	// The remote's copy no longer starts this machine's file. init cannot tell
	// that from another machine having this one's id, so it says what to run.
	if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil || !strings.Contains(err.Error(), "fleetd init --reclaim --dir \""+dir+"\"") {
		t.Fatalf("init again: %v, want an error naming the init --reclaim to run, with this journal's --dir", err)
	}
	stdout, _, err := exec(t, "init", "--json", "--dir", dir, remote)
	var out struct {
		SyncError string `json:"sync_error"`
	}
	if err == nil || json.Unmarshal([]byte(stdout), &out) != nil || !strings.Contains(out.SyncError, "--reclaim") {
		t.Fatalf("init --json again: %v, output %q; want an error, and JSON whose sync_error names --reclaim", err, stdout)
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--reclaim", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatalf("sync after init --reclaim: %v", err)
	}
	n, ok := remoteHostRecords(t, remote, "s", "recorded before init ran again")
	if !ok || n != 2 {
		t.Fatalf("the remote's file for this machine holds %d records (the new one: %v), want both", n, ok)
	}
}

// A journal directory restored from an older copy, git directory and all, has a
// HEAD behind the remote. --reclaim puts back what the remote has, not what that
// old HEAD had, and publishes the newer record after it.
func TestInitReclaimRepairsAJournalRestoredFromAnOlderCopy(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	record := func(note string) {
		t.Helper()
		if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", note); err != nil {
			t.Fatal(err)
		}
	}
	record("r1")
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err := os.CopyFS(backup, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	record("r2")
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(dir, os.DirFS(backup)); err != nil {
		t.Fatal(err)
	}
	record("r3")
	if _, _, err := exec(t, "sync", "--dir", dir); err == nil {
		t.Fatal("sync from the restored copy succeeded; it cannot publish r3 after r2")
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--reclaim", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{"r1", "r2", "r3"} {
		if n := remoteCount(t, remote, `"`+note+`"`); n != 1 {
			t.Errorf("%s is on the remote %d times, want once", note, n)
		}
	}
}

// When the remote's copy of this machine's file holds records another machine
// wrote, init does not adopt them: the sync reports the shared id.
func TestInitAgainDoesNotAdoptAnotherMachinesRecords(t *testing.T) {
	remote := emptyJournalRemote(t)
	t.Setenv("FLEET_SALT", "s")
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	// Another machine with this machine's id publishes. A cloned VM has this
	// machine's name too, so nothing in its records tells the two apart.
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", remote, seed)
	if _, _, err := exec(t, "record", "--dir", seed, "--type", "note", "--note", "the other machine"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "another machine")
	gitIn(t, seed, "push", "--quiet")

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "this machine"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("init with another machine's records under this id: %v", err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("sync with another machine's records under this id: %v", err)
	}
	if remoteHolds(t, remote, "main", "this machine") {
		t.Fatal("this machine's record was published into another machine's file")
	}
}

// A sync killed between moving another identity's tracked file aside and putting
// its published copy back leaves the file missing. The next sync puts it back.
func TestATrackedFileAKilledSyncLeftMissingIsPutBack(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	// Published under no salt, from a plain clone, as a machine did before init.
	recordWithout(t, dir, "--note", "published under no salt")
	plain := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	gitIn(t, dir, "add", plain)
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "published by hand")
	gitIn(t, dir, "push", "--quiet")
	if err := os.Remove(filepath.Join(dir, plain)); err != nil {
		t.Fatal(err)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, plain)); err != nil {
		t.Fatalf("the tracked file is still missing: %v", err)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after the sync:\n%s", out)
	}
}

// This machine's own file goes missing: an init --reclaim was killed between
// moving it aside and putting the remote's copy back. The next sync puts that
// copy back, rather than finding the remote holding records this machine lacks.
// The directory was restored from an older copy, so its HEAD is behind the
// remote, and only the remote's copy holds every published record.
func TestThisMachinesMissingFileIsPutBackAsTheRemoteHasIt(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "published first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err := os.CopyFS(backup, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "published later"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(dir, os.DirFS(backup)); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "fetch", "--quiet") // as init --reclaim does before its sync
	own := fleetFile(t, dir)
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatalf("sync with this machine's file missing: %v", err)
	}
	got, err := os.ReadFile(own)
	if err != nil || !strings.Contains(string(got), "published first") || !strings.Contains(string(got), "published later") {
		t.Fatalf("this machine's file holds %q (%v), want the remote's copy", got, err)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after the sync:\n%s", out)
	}
}

// When git cannot say whether another identity's file is tracked, or what was
// published under it (it timed out, say), nothing about that file is decided
// until it can: the file is neither moved nor re-filed, and a copy moved earlier
// that has grown since waits too. Taking it for untracked, or for published
// under nothing, would file its published records a second time.
func TestRefilingWaitsWhenGitCannotSay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script in place of git")
	}
	for _, tc := range []struct{ name, fails string }{
		{"anything about the file", ""},
		{"whether git tracks it", "ls-files"},
		{"what was published", "ls-tree|cat-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			// Published under no salt, from a plain clone, as a machine did before
			// init, then a record under that identity that was never published.
			recordWithout(t, dir, "--note", "published under no salt")
			plain := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
			gitIn(t, dir, "add", plain)
			gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "published by hand")
			gitIn(t, dir, "push", "--quiet")
			recordWithout(t, dir, "--note", "never published")
			fleetFileAged(t, dir)
			if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			// A process that had the file open appends to the copy moved aside, and
			// one that starts again writes a record into the file put back.
			keep := filepath.Join(dir, ".git", "fleetd-pre-init")
			copies := func() []string {
				t.Helper()
				m, err := filepath.Glob(filepath.Join(keep, plain+".*"))
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			moved := copies()
			if len(moved) != 1 {
				t.Fatalf("moved copies %v, want one", moved)
			}
			f, err := os.OpenFile(moved[0], os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(recordLine(t, "appended to the moved copy")); err != nil {
				t.Fatal(err)
			}
			f.Close()
			recordWithout(t, dir, "--note", "written after the move")

			real, err := osexec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			fail := "echo 'fatal: timed out' >&2; exit 128"
			script := "#!/bin/sh\ncase \"$*\" in *" + plain + "*) " + fail + ";; esac\nexec " + real + " \"$@\"\n"
			if tc.fails != "" {
				script = "#!/bin/sh\ncase \"$*\" in *" + plain + "*) for a in \"$@\"; do case \"$a\" in " + tc.fails + ") " + fail +
					";; esac; done;; esac\nexec " + real + " \"$@\"\n"
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			path := os.Getenv("PATH")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
			fleetFileAged(t, dir)
			// The sync itself may fail, since git fails on that file; what matters
			// is what it filed.
			_, _, _ = exec(t, "sync", "--dir", dir)
			t.Setenv("PATH", path)
			if n := remoteCount(t, remote, "published under no salt"); n != 1 {
				t.Fatalf("with git unable to say, the published record is on the remote %d times, want once", n)
			}
			if got, err := os.ReadFile(filepath.Join(dir, plain)); err != nil || !strings.Contains(string(got), "written after the move") {
				t.Fatalf("with git unable to say, the file holds %q (%v); want it left as it was", got, err)
			}
			if n := len(copies()); n != 1 {
				t.Fatalf("with git unable to say, the file was moved: %d moved copies, want one", n)
			}

			fleetFileAged(t, dir)
			if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			if n := remoteCount(t, remote, "published under no salt"); n != 1 {
				t.Fatalf("the published record is on the remote %d times, want once", n)
			}
			for _, note := range []string{"never published", "appended to the moved copy", "written after the move"} {
				if _, ok := remoteHostRecords(t, remote, "s", note); !ok {
					t.Errorf("once git could say, %q was still not filed", note)
				}
			}
			if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
				t.Fatalf("git status after the syncs:\n%s", out)
			}
		})
	}
}

// putBack never overwrites a file that exists, with a hard link or without one.
func TestPutBackNeverOverwrites(t *testing.T) {
	for _, links := range []bool{true, false} {
		t.Run(fmt.Sprintf("hard links %v", links), func(t *testing.T) {
			if !links {
				defer func(f func(string, string) error) { linkFile = f }(linkFile)
				linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: errors.ErrUnsupported} }
			}
			dir := t.TempDir()
			staged := filepath.Join(dir, "staged")
			if err := os.WriteFile(staged, []byte("published\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "host-x.jsonl")
			placed, err := putBack(staged, path, "published\n")
			if got, _ := os.ReadFile(path); err != nil || !placed || string(got) != "published\n" {
				t.Fatalf("missing file: placed %v, err %v, content %q", placed, err, got)
			}
			if err := os.WriteFile(path, []byte("a record written meanwhile\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			placed, err = putBack(staged, path, "published\n")
			if got, _ := os.ReadFile(path); err != nil || placed || string(got) != "a record written meanwhile\n" {
				t.Fatalf("existing file: placed %v, err %v, content %q", placed, err, got)
			}
		})
	}
}

// Without hard links, putBack creates the file and then writes the copy. A
// record a process appends between the two is kept.
func TestPutBackKeepsARecordAppendedAsItCreatesTheFile(t *testing.T) {
	defer func(f func(string, string) error) { linkFile = f }(linkFile)
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: errors.ErrUnsupported} }
	defer func(f func(string) (*os.File, error)) { createFile = f }(createFile)
	create := createFile
	createFile = func(path string) (*os.File, error) {
		f, err := create(path)
		if err == nil {
			w, werr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if werr != nil {
				t.Fatal(werr)
			}
			if _, werr := w.WriteString("a record appended meanwhile\n"); werr != nil {
				t.Fatal(werr)
			}
			w.Close()
		}
		return f, err
	}
	dir := t.TempDir()
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, []byte("published\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "host-x.jsonl")
	placed, err := putBack(staged, path, "published\n")
	if got, _ := os.ReadFile(path); err != nil || !placed || !strings.Contains(string(got), "a record appended meanwhile\n") {
		t.Fatalf("placed %v, err %v, content %q: the appended record is lost", placed, err, got)
	}
}

// Windows' clock can give two moves the same time. A process that starts the file
// again before its published copy is back makes the sync move that file too; its
// copy must not replace the first, which holds the record never published, and
// the published copy is still back by the end of that sync.
func TestASecondMoveInTheSameClockTickKeepsTheFirstCopy(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	recordWithout(t, dir, "--note", "published under no salt")
	plain := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	gitIn(t, dir, "add", plain)
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "published by hand")
	gitIn(t, dir, "push", "--quiet")
	recordWithout(t, dir, "--note", "never published")
	window := recordLine(t, "written in the window")

	defer func(f func() time.Time) { now = f }(now)
	fixed := time.Now()
	now = func() time.Time { return fixed }
	defer func(f func(string, string) error) { linkFile = f }(linkFile)
	link, started := linkFile, false
	linkFile = func(oldname, newname string) error {
		if !started && filepath.Base(newname) == plain {
			started = true
			if err := os.WriteFile(newname, []byte(window), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return link(oldname, newname)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("no process started the file again")
	}
	got, err := os.ReadFile(filepath.Join(dir, plain))
	if err != nil || !strings.Contains(string(got), "published under no salt") || strings.Contains(string(got), "written in the window") {
		t.Fatalf("after the sync the file holds %q (%v), want its published copy", got, err)
	}
	for _, note := range []string{"never published", "written in the window"} {
		if _, ok := remoteHostRecords(t, remote, "s", note); !ok {
			t.Errorf("%q is on no remote file", note)
		}
	}
	if n := remoteCount(t, remote, "published under no salt"); n != 1 {
		t.Errorf("the published record is on the remote %d times, want once", n)
	}
}

// A record close to the size limit would grow past it when re-filing marks it
// with refiled.from. It is filed all the same, unmarked, and it stops nothing:
// the records around it, and later ones, are filed too.
func TestARecordNearTheSizeLimitIsStillFiled(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	// A record of about 4085 bytes, newline included: within the limit as
	// written, past it with the marker.
	long := "long " + strings.Repeat("x", journal.MaxRecordBytes-11-len(recordLine(t, "x"))+1)
	for _, note := range []string{"first, before the long one", long, "third, after the long one"} {
		recordWithout(t, dir, "--note", note)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	// A process that had the file open appends to the moved copy, so the next
	// sync reads it again, the long record included.
	plain := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	moved, err := filepath.Glob(filepath.Join(dir, ".git", "fleetd-pre-init", plain+".*"))
	if err != nil || len(moved) != 1 {
		t.Fatalf("moved copies %v (%v), want one", moved, err)
	}
	f, err := os.OpenFile(moved[0], os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(recordLine(t, "appended to the moved copy")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	recordWithout(t, dir, "--note", "fourth, written later")
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{"first, before the long one", long, "third, after the long one", "appended to the moved copy", "fourth, written later"} {
		if _, ok := remoteHostRecords(t, remote, "s", note); !ok {
			t.Errorf("%.30q is on no remote file", note)
		}
	}
	if n := remoteCount(t, remote, long); n != 1 {
		t.Errorf("the long record is on the remote %d times, want once", n)
	}
}

// A line too large to file even unmarked, from a process that does not keep to
// the limit, is skipped: it stops nothing, and the records after it are filed.
func TestARecordTooLargeEvenUnmarkedIsSkipped(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	plain := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	huge := strings.Replace(recordLine(t, "x"), `"note":"x"`, `"note":"`+strings.Repeat("y", journal.MaxRecordBytes)+`"`, 1)
	if err := os.WriteFile(filepath.Join(dir, plain), []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	recordWithout(t, dir, "--note", "after the huge one")
	fleetFileAged(t, dir)
	if _, stderr, err := exec(t, "sync", "--dir", dir); err != nil || !strings.Contains(stderr, "1 record") {
		t.Fatalf("sync: %v; stderr %q, want a warning naming the record not filed", err, stderr)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "after the huge one"); !ok {
		t.Fatal("the record after the huge one was never filed")
	}
}

// A record written with FLEET_SALT before the journal had fleetd.json is filed
// under the fleet's id even when FLEET_SALT is gone by the time init runs.
func TestARecordWrittenUnderFleetSaltBeforeInitIsFiledWithoutIt(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	t.Setenv("FLEET_SALT", "a-salt-of-its-own")
	for range 2 {
		if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "written under FLEET_SALT"); err != nil {
			t.Fatal(err)
		}
	}
	if got := notedSalts(dir); !slices.Equal(got, []string{"a-salt-of-its-own"}) {
		t.Fatalf("the salts note holds %q, want the salt once", got)
	}
	t.Setenv("FLEET_SALT", "")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "written under FLEET_SALT"); !ok {
		t.Fatal("a record written under FLEET_SALT before init is on no remote file")
	}
}

// Re-filing a long backlog stops when the sync's time runs out, files nothing
// more, and reports nothing read, so the next pass reads the copy again.
func TestRefilingStopsWhenItsTimeRunsOut(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backlog := filepath.Join(t.TempDir(), "moved")
	data := recordLine(t, "one") + recordLine(t, "two")
	if err := os.WriteFile(backlog, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	from, me := hostID(t, "--salt", "", "--dir", t.TempDir()), identity("s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if added, read, _, err := refileOne(ctx, store, backlog, from, me, map[string]bool{}, map[string]bool{}); !errors.Is(err, context.Canceled) || added != 0 || read != 0 {
		t.Fatalf("with its time run out: %d filed, %d bytes read, err %v; want none, and the cancellation", added, read, err)
	}
	if added, read, _, err := refileOne(context.Background(), store, backlog, from, me, map[string]bool{}, map[string]bool{}); err != nil || added != 2 || read != int64(len(data)) {
		t.Fatalf("with time: %d filed, %d bytes read, err %v; want both records and the whole copy", added, read, err)
	}
}

// A salt is noted exactly as given, a trailing carriage return included, so the
// records written under it are found again.
func TestASaltEndingInACarriageReturnIsNotedExactly(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	t.Setenv("FLEET_SALT", "abc\r")
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "written under a salt ending in CR"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "")
	if got := notedSalts(dir); !slices.Equal(got, []string{"abc\r"}) {
		t.Fatalf("the salts note holds %q, want \"abc\\r\"", got)
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "written under a salt ending in CR"); !ok {
		t.Fatal("a record written under a salt ending in CR is on no remote file")
	}
}

// A record written under FLEET_SALT while the journal's fleetd.json is unusable
// was written under FLEET_SALT, not the journal's salt, and is noted as such.
func TestARecordWrittenBesideAnUnusableFleetFileIsFiled(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "x")
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "written beside an unusable fleetd.json"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "written beside an unusable fleetd.json"); !ok {
		t.Fatal("the record is on no remote file")
	}
}

// A journal directory at the root of a filesystem has nothing beside it, and a
// note inside it would be a file init refuses; it gets none.
func TestAJournalAtAFilesystemRootGetsNoSaltsNote(t *testing.T) {
	if got := saltsNote(string(filepath.Separator)); got != "" {
		t.Fatalf("saltsNote(%q) = %q, want none", string(filepath.Separator), got)
	}
}

// A record an older fleetd wrote under FLEET_SALT has no note of its salt. A sync
// that still has FLEET_SALT set files it.
func TestARecordUnderAnUnnotedFleetSaltIsFiledWhileItIsSet(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	t.Setenv("FLEET_SALT", "an-older-salt")
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "written by an older fleetd"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(saltsNote(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "written by an older fleetd"); !ok {
		t.Fatal("a record written under FLEET_SALT is on no remote file while FLEET_SALT is set")
	}
}

// Re-filing runs under the sync's lock: a sync that cannot take it, because
// another sync of the journal is running, moves and files nothing.
func TestASyncThatCannotTakeTheLockRefilesNothing(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	recordWithout(t, dir, "--note", "waits for the lock")
	plain := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	lock := filepath.Join(dir, ".git", "fleetd-sync.lock")
	if err := os.WriteFile(lock, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); !errors.Is(err, gitsync.ErrBusy) {
		t.Fatalf("sync with the lock held: %v, want ErrBusy", err)
	}
	if _, err := os.Stat(filepath.Join(dir, plain)); err != nil {
		t.Fatalf("a sync without the lock moved the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "fleetd-pre-init")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a sync without the lock re-filed: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "waits for the lock"); !ok {
		t.Fatal("once the lock was free, the record was still not filed")
	}
}

// where's last_published is the time of the newest record the remote has from a
// machine, even when records re-filed after it were written earlier; while
// every record it has from the machine was re-filed, it is the last of those.
func TestLastPublishedIsTheNewestRecordNotTheLastRefiledOne(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	lastPublished := func() time.Time {
		t.Helper()
		stdout, _, err := exec(t, "where", "--dir", dir, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var hosts []struct {
			Host          string `json:"host"`
			LastPublished string `json:"last_published"`
		}
		if err := json.Unmarshal([]byte(stdout), &hosts); err != nil {
			t.Fatalf("where --json: %v\n%s", err, stdout)
		}
		me := strings.ReplaceAll(hostID(t, "--dir", dir), ":", "-")
		for _, h := range hosts {
			if h.Host == me {
				at, err := time.Parse(time.RFC3339, h.LastPublished)
				if err != nil {
					t.Fatalf("last_published = %q: %v", h.LastPublished, err)
				}
				return at
			}
		}
		t.Fatalf("where --json lists no entry for this machine:\n%s", stdout)
		return time.Time{}
	}
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).UTC().Truncate(time.Second)
	recordWithout(t, dir, "--note", "two weeks ago", "--at", weekAgo.Add(-7*24*time.Hour).Format(time.RFC3339))
	recordWithout(t, dir, "--note", "a week ago", "--at", weekAgo.Format(time.RFC3339))
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if at := lastPublished(); !at.Equal(weekAgo) {
		t.Fatalf("with only a re-filed record published, last_published = %v, want %v", at, weekAgo)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "current work"); err != nil {
		t.Fatal(err)
	}
	recordWithout(t, dir, "--note", "also a week ago", "--at", weekAgo.Format(time.RFC3339))
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if at := lastPublished(); time.Since(at) > time.Hour {
		t.Fatalf("last_published = %v, want the time of the record just published", at)
	}
}

// A journal directory may hold a fleetd.json with another salt, from an earlier
// setup. The journal's salt wins, and the records written under the other one
// are published under the journal's.
func TestInitReplacesALocalFleetFileAndPublishesItsRecords(t *testing.T) {
	for _, given := range []string{"", "s2"} {
		t.Run("salt "+given, func(t *testing.T) {
			remote := emptyJournalRemote(t)
			if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "other"), "--salt", "s2", remote); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "journal")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "s1"}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "under the local salt"); err != nil {
				t.Fatal(err)
			}
			args := []string{"init", "--dir", dir}
			if given != "" {
				args = append(args, "--salt", given)
			}
			if _, _, err := exec(t, append(args, remote)...); err != nil {
				t.Fatalf("init over a local fleetd.json with another salt: %v", err)
			}
			if fleet, ok, err := gitsync.ReadFleet(dir); err != nil || !ok || fleet.Salt != "s2" {
				t.Fatalf("fleetd.json is %+v (%v, %v), want the journal's", fleet, ok, err)
			}
			fleetFileAged(t, dir)
			if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			if _, ok := remoteHostRecords(t, remote, "s2", "under the local salt"); !ok {
				t.Fatal("the record written under the local fleetd.json's salt was not published under the journal's")
			}
		})
	}
}

// The journal's salt can change while a hook still holds the old one. Its
// record is published under the new salt.
func TestARecordUnderTheSaltTheJournalReplacedIsPublished(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s1", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "published under s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(t.TempDir(), "editor")
	gitIn(t, filepath.Dir(editor), "clone", "--quiet", remote, editor)
	if err := os.WriteFile(filepath.Join(editor, gitsync.FleetFile), []byte(`{"salt": "s2"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, editor, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-am", "a new salt")
	gitIn(t, editor, "push", "--quiet")
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	// A hook that read fleetd.json just before that sync replaced it.
	current, err := os.ReadFile(filepath.Join(dir, gitsync.FleetFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "s1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "a hook that held s1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), current, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		fleetFileAged(t, dir)
		if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := remoteHostRecords(t, remote, "s2", "a hook that held s1"); !ok {
		t.Fatal("the record written under the replaced salt was not published under the new one")
	}
}

// A record filed late from another identity is older than its place in the
// file: where does not report it as this machine's latest activity.
func TestWhereLeavesARefiledRecordOutOfTheLatestActivity(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	recordWithout(t, dir, "--note", "before init", "--at", "2026-09-01T10:00:00Z")
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "current work", "--at", "2026-10-01T17:00:00Z"); err != nil {
		t.Fatal(err)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []whereEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil || len(entries) != 1 {
		t.Fatalf("where --json: %v\n%s", err, stdout)
	}
	e := entries[0]
	if e.LastNote != "current work" || e.TimestampsOutOfOrder || e.Records != 2 {
		t.Fatalf("where reports last %q (out of order: %v, %d records), want current work, in order, 2 records", e.LastNote, e.TimestampsOutOfOrder, e.Records)
	}
}
