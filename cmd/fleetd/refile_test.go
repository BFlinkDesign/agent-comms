package main

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
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
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after the syncs:\n%s", out)
	}
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
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatalf("sync after init again: %v", err)
	}
	n, ok := remoteHostRecords(t, remote, "s", "recorded before init ran again")
	if !ok || n != 2 {
		t.Fatalf("the remote's file for this machine holds %d records (the new one: %v), want both", n, ok)
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
	// Another machine with this machine's id, under another name, publishes.
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", remote, seed)
	if _, _, err := exec(t, "record", "--dir", seed, "--type", "note", "--note", "the other machine"); err != nil {
		t.Fatal(err)
	}
	own := filepath.Base(fleetFile(t, dir))
	data, err := os.ReadFile(filepath.Join(seed, own))
	if err != nil {
		t.Fatal(err)
	}
	name := hostName(t)
	if err := os.WriteFile(filepath.Join(seed, own), []byte(strings.ReplaceAll(string(data), `"`+name+`"`, `"another-pc"`)), 0o644); err != nil {
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
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("sync with another machine's records under this id: %v", err)
	}
	if remoteHolds(t, remote, "main", "this machine") {
		t.Fatal("this machine's record was published into another machine's file")
	}
}

func hostName(t *testing.T) string {
	t.Helper()
	stdout, _, err := exec(t, "host", "--json", "--dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var h hostOut
	if err := json.Unmarshal([]byte(stdout), &h); err != nil {
		t.Fatal(err)
	}
	return h.Name
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
