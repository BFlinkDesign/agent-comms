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
	"testing"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
)

// emptyJournalRemote is a bare repository with no commits, as GitHub creates one,
// and a clean git configuration, so the tests see what a fresh PC sees.
func emptyJournalRemote(t *testing.T) string {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("FLEET_SALT", "")
	root := t.TempDir()
	remote := filepath.Join(root, "journal.git")
	gitIn(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	return remote
}

func hostID(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _, err := exec(t, append([]string{"host", "--json"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	var h hostOut
	if err := json.Unmarshal([]byte(stdout), &h); err != nil {
		t.Fatal(err)
	}
	return h.ID
}

// A hook started without FLEET_SALT in its environment, as one started by a
// program launched before the variable was set would be, still files its records
// under the fleet's id: the salt comes from the journal.
func TestWithoutFleetSaltTheJournalsSaltStillGivesTheFleetsID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "the-fleets"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "")
	want := hostID(t, "--salt", "the-fleets", "--dir", t.TempDir())
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "from a hook without FLEET_SALT"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, strings.ReplaceAll(want, ":", "-")+".jsonl")); err != nil {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("the record is not in the fleet id's file %s.jsonl: %v; the directory holds %v", want, err, entries)
	}
	if got := hostID(t, "--dir", dir); got != want {
		t.Fatalf("host --dir gives %s, want the fleet's %s", got, want)
	}
}

// --salt that contradicts the journal's is a mistake made on purpose, and is
// refused before anything is written.
func TestAnExplicitSaltThatContradictsTheJournalsIsRefusedAndNothingIsWritten(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "the-fleets"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "")
	for _, args := range [][]string{
		{"record", "--dir", dir, "--salt", "another", "--type", "note", "--note", "should not be written"},
		{"sync", "--dir", dir, "--salt", "another"},
		{"host", "--dir", dir, "--salt", "another"},
	} {
		if _, _, err := exec(t, args...); !errors.Is(err, gitsync.ErrSaltMismatch) || !strings.Contains(err.Error(), "--salt") {
			t.Fatalf("%s: err = %v, want a salt mismatch naming --salt", args[0], err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the journal directory holds %d entries, want only fleetd.json", len(entries))
	}
	// Repeating the journal's salt is fine.
	if _, _, err := exec(t, "host", "--dir", dir, "--salt", "the-fleets"); err != nil {
		t.Fatal(err)
	}
}

// A FLEET_SALT left over from before the journal had fleetd.json would otherwise
// stop every hook on the machine from recording: the journal's salt is used, and
// the stale one is named.
func TestAStaleFleetSaltStillRecordsUnderTheJournalsSalt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "the-fleets"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "a-stale-one")
	_, stderr, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "recorded all the same")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "FLEET_SALT differs") {
		t.Errorf("stderr does not name the stale FLEET_SALT: %q", stderr)
	}
	t.Setenv("FLEET_SALT", "")
	want := strings.ReplaceAll(hostID(t, "--salt", "the-fleets", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
		t.Fatalf("the record is not in the fleet id's file %s: %v", want, err)
	}
}

func TestInitSetsUpTheJournalAndEveryMachineGetsTheSameSalt(t *testing.T) {
	remote := emptyJournalRemote(t)
	a, b := filepath.Join(t.TempDir(), "a", "journal"), filepath.Join(t.TempDir(), "b", "journal")
	stdout, _, err := exec(t, "init", "--dir", a, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "started the journal") {
		t.Fatalf("stdout = %q", stdout)
	}
	if stdout, _, err = exec(t, "init", "--dir", b, remote); err != nil || strings.Contains(stdout, "started") {
		t.Fatalf("second machine: %q, %v", stdout, err)
	}
	if _, err := os.Stat(filepath.Join(b, gitsync.FleetFile)); err != nil {
		t.Fatalf("the second machine's journal has no %s: %v", gitsync.FleetFile, err)
	}
	if hostID(t, "--dir", a) != hostID(t, "--dir", b) {
		t.Fatal("two clones of one journal on one machine give two host ids")
	}
	// Ready to record and sync with no salt in the environment, and to be told
	// so a second time without harm.
	if _, _, err := exec(t, "record", "--dir", a, "--type", "note", "--note", "first record after init"); err != nil {
		t.Fatal(err)
	}
	if stdout, _, err := exec(t, "sync", "--dir", a); err != nil || !strings.Contains(stdout, "published 1 record") {
		t.Fatalf("sync: %q, %v", stdout, err)
	}
	if stdout, _, err := exec(t, "init", "--dir", a, remote); err != nil || !strings.Contains(stdout, "already set up") {
		t.Fatalf("init again: %q, %v", stdout, err)
	}
}

// A hook that fired before the journal was set up wrote its records into the
// journal directory. init keeps them, in the clone, and the next sync publishes
// them; nothing else is left behind.
func TestInitKeepsRecordsWrittenBeforeIt(t *testing.T) {
	remote := emptyJournalRemote(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "journal")
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "s", "--type", "note", "--note", "written before the journal existed"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "published 1 record") {
		t.Fatalf("stdout = %q", stdout)
	}
	if stdout, _, _ := exec(t, "where", "--dir", dir); !strings.Contains(stdout, "written before the journal existed") {
		t.Fatalf("where lost the record: %q", stdout)
	}
	// Beside the journal there is only the note of the salt the record was
	// written under, never a clone init made on the way.
	entries, _ := os.ReadDir(parent)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"journal", "journal.salts"}) {
		t.Fatalf("init left %v beside the journal", names)
	}
}

func TestInitLeavesADirectoryWithOtherFilesAlone(t *testing.T) {
	remote := emptyJournalRemote(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "journal")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("err = %v, want a refusal naming notes.txt", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the directory now holds %d entries", len(entries))
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Fatalf("init left something beside the directory: %v", entries)
	}
}

// A machine whose syncs fail must not look idle: where says the last sync
// failed, and which records have not been published.
func TestWhereSaysWhenThisMachinesSyncFailedAndWhatIsUnpublished(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "published before the remote went away"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "where", "--dir", dir)
	if err != nil || !strings.Contains(stdout, "as of this machine's last sync") || strings.Contains(stdout, "NOT PUBLISHED") {
		t.Fatalf("after a good sync: %q, %v", stdout, err)
	}

	gitIn(t, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "never published"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir, "--timeout", "20s"); err == nil {
		t.Fatal("a sync against a missing remote succeeded")
	}
	stdout, _, err = exec(t, "where", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"last sync", "FAILED", "as of its last successful sync", "NOT PUBLISHED: 1 record"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("where does not say %q:\n%s", want, stdout)
		}
	}
	stdout, _, err = exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []whereEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil || len(entries) != 1 {
		t.Fatalf("where --json: %v\n%s", err, stdout)
	}
	if entries[0].Unpublished != 1 || entries[0].LastPublished == "" {
		t.Fatalf("where --json: unpublished %d, last_published %q; want 1 and a time", entries[0].Unpublished, entries[0].LastPublished)
	}
}

// fleetFileAged makes the journal's fleetd.json old enough that a sync files the
// records this machine wrote under another identity, as it is on any machine
// past the moment init put it there.
func fleetFileAged(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(dir, gitsync.FleetFile), old, old); err != nil {
		t.Fatal(err)
	}
}

// remoteHostRecords counts the records the remote holds in the fleet identity's
// file, and says whether one carries note.
func remoteHostRecords(t *testing.T, remote, salt, note string) (int, bool) {
	t.Helper()
	name := strings.ReplaceAll(hostID(t, "--salt", salt, "--dir", t.TempDir()), ":", "-") + ".jsonl"
	out, err := osexec.Command("git", "--git-dir", remote, "show", "main:"+name).CombinedOutput()
	if err != nil {
		return 0, false
	}
	return strings.Count(string(out), "\n"), strings.Contains(string(out), note)
}

// A hook that fires while init is cloning appends to the journal directory under
// whatever salt it has. Init used to list the directory before cloning and delete
// it after, taking that record with it; now nothing in the directory is moved or
// removed, and the record is published under the fleet's id.
func TestInitKeepsARecordAHookAppendsWhileItRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slows git down with a shell script wrapper")
	}
	remote := emptyJournalRemote(t)
	real, err := osexec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	cloning := filepath.Join(t.TempDir(), "cloning")
	wrapper := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = clone ] && { : > " + cloning + "; sleep 2; }; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := filepath.Join(t.TempDir(), "journal")

	done := make(chan error, 1)
	go func() {
		_, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote)
		done <- err
	}()
	for i := 0; ; i++ {
		if _, err := os.Stat(cloning); err == nil {
			break
		}
		if i > 200 {
			t.Fatal("init never started cloning")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The hook has no salt: it fired before the journal's was known.
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "appended during init"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n, ok := remoteHostRecords(t, remote, "s", "appended during init"); !ok {
		t.Fatalf("the remote's file for this machine holds %d records and not the one appended during init", n)
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if len(entries) != 1 {
		t.Fatalf("init left %v beside the journal", entries)
	}
}

// A journal its machines already record into has a salt, whether or not
// fleetd.json says so. Without one given, init refuses rather than invent another.
func TestInitWithoutASaltRefusesAJournalThatHoldsRecords(t *testing.T) {
	remote := emptyJournalRemote(t)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "host-0123456789abcdef.jsonl"), []byte(`{"id":"hive:1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "a machine's records")
	gitIn(t, seed, "push", "--quiet", "-u", "origin", "main")

	_, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), remote)
	if !errors.Is(err, gitsync.ErrNeedSalt) || !strings.Contains(err.Error(), "FLEET_SALT") || !strings.Contains(err.Error(), "ran without one") {
		t.Fatalf("err = %v, want ErrNeedSalt saying how to give the salt, and what to do if the fleet had none", err)
	}
	t.Setenv("FLEET_SALT", "the-fleets")
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), remote); err != nil {
		t.Fatal(err)
	}
	out, err := osexec.Command("git", "--git-dir", remote, "show", "main:"+gitsync.FleetFile).CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"salt": "the-fleets"`) {
		t.Fatalf("the journal's fleetd.json is %q (%v), want FLEET_SALT's salt", out, err)
	}
}

// A hook that ran before init filed its records under this machine's id with no
// salt. Nothing would ever publish those: a sync files them under the fleet's id
// and keeps the original file in the clone's git directory.
func TestRecordsWrittenBeforeInitUnderAnotherSaltArePublished(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "before the salt was known"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "before the salt was known"); !ok {
		t.Fatal("the remote's file for this machine lacks the record written before init")
	}
	unsalted := strings.ReplaceAll(hostID(t, "--salt", "", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	if _, err := os.Stat(filepath.Join(dir, unsalted)); !os.IsNotExist(err) {
		t.Fatalf("%s is still in the journal: %v", unsalted, err)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, ".git", "fleetd-pre-init", unsalted+".*"))
	if len(kept) != 1 {
		t.Fatalf("the original is not kept in .git/fleetd-pre-init: %v", kept)
	}
	// A sync later on finds nothing more to file, and publishes nothing twice.
	fleetFileAged(t, dir)
	if stdout, _, err := exec(t, "sync", "--dir", dir); err != nil || !strings.Contains(stdout, "published 0 records") {
		t.Fatalf("sync after init: %q, %v", stdout, err)
	}
}

func TestInitSetsUpAnExistingEmptyDirectory(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
}

// An invalid fleetd.json reaching a machine by sync must not stop it recording,
// nor stop the sync that brings in the fix.
func TestAnInvalidFleetFileDoesNotStopRecordingOrSyncing(t *testing.T) {
	remote := emptyJournalRemote(t)
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, dir := range []string{a, b} {
		if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
			t.Fatal(err)
		}
	}
	editor := filepath.Join(t.TempDir(), "editor")
	gitIn(t, filepath.Dir(editor), "clone", "--quiet", remote, editor)
	edit := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(editor, gitsync.FleetFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, editor, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-am", "edit")
		gitIn(t, editor, "push", "--quiet")
	}
	edit(`{"salt": "s",}`)
	if _, _, err := exec(t, "sync", "--dir", b); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := exec(t, "record", "--dir", b, "--type", "note", "--note", "with a broken fleetd.json")
	if err != nil || !strings.Contains(stderr, "not valid JSON") {
		t.Fatalf("record with a broken fleetd.json: %v, stderr %q", err, stderr)
	}
	edit(`{"salt": "s"}` + "\n")
	if _, _, err := exec(t, "sync", "--dir", b); err != nil {
		t.Fatalf("the sync that brings the fix: %v", err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "with a broken fleetd.json"); !ok {
		t.Fatal("the record made while fleetd.json was broken was not published under the fleet's id")
	}
}

// A sync that cannot even start has failed, and where says so.
func TestWhereSaysASyncThatCouldNotStartFailed(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir, "--salt", "another"); err == nil {
		t.Fatal("a sync with a contradicting --salt succeeded")
	}
	stdout, _, err := exec(t, "where", "--dir", dir)
	if err != nil || !strings.Contains(stdout, "FAILED") {
		t.Fatalf("where: %v\n%s", err, stdout)
	}
}

// A journal directory inside another repository, such as dotfiles kept in the
// home directory, is not a clone of the journal, and where claims nothing about
// what the journal has published.
func TestWhereInsideAnotherRepositoryClaimsNothingAboutPublishing(t *testing.T) {
	dotfiles := emptyJournalRemote(t)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", dotfiles, seed)
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "dotfiles")
	gitIn(t, seed, "push", "--quiet", "-u", "origin", "main")
	home := filepath.Join(t.TempDir(), "home")
	gitIn(t, filepath.Dir(home), "clone", "--quiet", dotfiles, home)
	dir := filepath.Join(home, ".ai", "channels", "journal")
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "s", "--type", "note", "--note", "inside dotfiles"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "where", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range []string{"published", "NOT PUBLISHED", "no sync of this journal is noted"} {
		if strings.Contains(stdout, claim) {
			t.Errorf("where says %q about a journal that is not a clone:\n%s", claim, stdout)
		}
	}
}

func TestInitRefusesACloneOfAnotherRepository(t *testing.T) {
	one, two := emptyJournalRemote(t), emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", one); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", two); !errors.Is(err, gitsync.ErrOtherRemote) {
		t.Fatalf("err = %v, want ErrOtherRemote", err)
	}
}

// Two records written with the same --at are the same bytes. With one of them
// published, the other is still unpublished.
func TestWhereCountsIdenticalRecordsAsUnpublished(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	same := []string{"record", "--dir", dir, "--type", "note", "--note", "twice", "--at", "2026-10-01T12:00:00Z"}
	if _, _, err := exec(t, same...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, same...); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []whereEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil || len(entries) != 1 || entries[0].Unpublished != 1 {
		t.Fatalf("where --json: %v\n%s", err, stdout)
	}
}

func TestInitTakesARelativePathToALocalRepository(t *testing.T) {
	remote := emptyJournalRemote(t)
	t.Chdir(filepath.Dir(remote))
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), "--salt", "s", "./"+filepath.Base(remote)); err != nil {
		t.Fatal(err)
	}
}

// recordsOn makes the remote's only branch the given one, holding a machine's
// records but no fleetd.json, while the remote's HEAD names main.
func recordsOn(t *testing.T, remote string, branches ...string) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", remote, seed)
	for _, b := range branches {
		gitIn(t, seed, "checkout", "--quiet", "-B", b)
		if err := os.WriteFile(filepath.Join(seed, "host-0123456789abcdef.jsonl"), []byte(`{"id":"hive:`+b+`"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, seed, "add", ".")
		gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "records on "+b)
		gitIn(t, seed, "push", "--quiet", "origin", b)
	}
}

func remoteBranchList(t *testing.T, remote string) string {
	t.Helper()
	out, err := osexec.Command("git", "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// A repository whose default branch does not exist, with the fleet's records on
// its only branch, is not empty: init follows that branch. It must not start a
// second branch with a salt of its own, nor tell the person to delete one.
func TestInitFollowsTheOnlyBranchOfARepositoryWhoseDefaultIsMissing(t *testing.T) {
	remote := emptyJournalRemote(t) // HEAD names main, which never gets a commit
	recordsOn(t, remote, "master")
	before := remoteBranchList(t, remote)

	_, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), remote)
	if after := remoteBranchList(t, remote); after != before {
		t.Errorf("init pushed to a repository that holds records: %q became %q", before, after)
	}
	if !errors.Is(err, gitsync.ErrNeedSalt) {
		t.Fatalf("err = %v, want ErrNeedSalt: the records on master have a salt init must be given", err)
	}
	stdout, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), "--salt", "s", remote)
	if err != nil || !strings.Contains(stdout, "following master") {
		t.Fatalf("init with the salt: %v\n%s", err, stdout)
	}
	if after := remoteBranchList(t, remote); after != before {
		t.Errorf("the journal is on %q, want master alone", after)
	}
}

// With several branches and no default among them, the journal could be on any:
// init refuses, before anything is pushed, and names them.
func TestInitRefusesSeveralBranchesWithoutADefault(t *testing.T) {
	remote := emptyJournalRemote(t)
	recordsOn(t, remote, "master", "trunk")
	before := remoteBranchList(t, remote)
	_, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), "--salt", "s", remote)
	if !errors.Is(err, gitsync.ErrNoDefaultBranch) || !strings.Contains(err.Error(), "master, trunk") {
		t.Fatalf("err = %v, want ErrNoDefaultBranch naming master and trunk", err)
	}
	if after := remoteBranchList(t, remote); after != before {
		t.Errorf("init pushed: %q became %q", before, after)
	}
}

// init stopped right after the clone's git directory moved in. Whether a hook's
// sync runs before init is run again or not, afterwards the clone is one a sync
// keeps in step with the remote, holding every machine's records.
func TestRerunningAnInterruptedInitFinishesIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script in place of git")
	}
	for _, syncFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("a sync before the rerun %v", syncFirst), func(t *testing.T) {
			remote := emptyJournalRemote(t)
			other := filepath.Join(t.TempDir(), "other")
			if _, _, err := exec(t, "init", "--dir", other, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			// Another machine's records, so the remote has a file a sync brings in.
			if err := os.WriteFile(filepath.Join(other, "host-0123456789abcdef.jsonl"), []byte(`{"id":"hive:1"}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, other, "add", "host-0123456789abcdef.jsonl")
			gitIn(t, other, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "another machine")
			gitIn(t, other, "push", "--quiet")
			real, err := osexec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = read-tree ] && { echo interrupted >&2; exit 1; }; done\nexec " + real + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "journal")
			path := os.Getenv("PATH")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
			if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil {
				t.Fatal("the interruption did not happen")
			}
			t.Setenv("PATH", path)
			if syncFirst {
				if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
					t.Fatal(err)
				}
			}
			again, _, err := exec(t, "init", "--json", "--dir", dir, remote)
			if err != nil {
				t.Fatal(err)
			}
			var res struct {
				Restored []string `json:"restored"`
			}
			if err := json.Unmarshal([]byte(again), &res); err != nil {
				t.Fatalf("init --json: %v\n%s", err, again)
			}
			// With no sync in between, init itself checks out what the interrupted one
			// did not, so the clone is whole even if init's own sync fails.
			if !syncFirst && !slices.Contains(res.Restored, "host-0123456789abcdef.jsonl") {
				t.Fatalf("init ran again restored %q, want another machine's file among them", res.Restored)
			}
			stdout, _, err := exec(t, "sync", "--dir", dir)
			if err != nil || strings.Contains(stdout, "kept") {
				t.Fatalf("after init ran again, sync says: %q (%v)", stdout, err)
			}
			if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
				t.Fatalf("after init ran again, git status says:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(dir, "host-0123456789abcdef.jsonl")); err != nil {
				t.Fatalf("after init ran again, another machine's records are missing: %v", err)
			}
		})
	}
}

// An init killed before it finished leaves its temporary clone beside the
// journal. The next init removes it, but not one an init still running may own.
func TestInitRemovesAnAbandonedCloneButNotOneInUse(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	abandoned, current, notes := dir+".init-1111", dir+".init-2222", dir+".init-notes"
	// Not init's either: a name MkdirTemp never gives, and a clone holding more
	// than a git directory.
	named, more := dir+".init-backup", dir+".init-3333"
	for _, d := range []string{abandoned, current, named, more} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(notes, "todo.txt"), filepath.Join(more, "todo.txt")} {
		if err := os.WriteFile(f, []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for _, d := range []string{abandoned, notes, named, more} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("the abandoned clone is still there: %v", err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Errorf("a clone an init may still be using was removed: %v", err)
	}
	for _, kept := range []string{filepath.Join(notes, "todo.txt"), filepath.Join(named, ".git"), filepath.Join(more, "todo.txt")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("a directory init did not make was removed: %v", err)
		}
	}
}

// A push fails during init's sync. With --reclaim, nothing may have been put
// back, and no later sync reclaims, so init fails and says to run it again.
// Without it, the next sync retries what failed, and init says so.
func TestInitReclaimWhoseSyncFailedSaysToRunItAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script in place of git")
	}
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "waits for a push"); err != nil {
		t.Fatal(err)
	}
	real, err := osexec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = push ] && { echo 'fatal: unable to access the remote' >&2; exit 128; }; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "init", "--dir", dir, "--reclaim", remote); err == nil || !strings.Contains(err.Error(), "run the same `fleetd init --reclaim` command again") {
		t.Fatalf("init --reclaim whose sync failed: %v, want an error saying to run the same command again", err)
	}
	stdout, _, err := exec(t, "init", "--dir", dir, remote)
	if err != nil || !strings.Contains(stdout, "the next one retries") {
		t.Fatalf("init whose sync failed: %v, %q; want success, saying the next sync retries", err, stdout)
	}
	t.Setenv("PATH", path)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "waits for a push"); !ok {
		t.Fatal("once pushing worked, the record was still not published")
	}
}

// A sync error no later sync gets past, a commit made by hand in the clone or a
// push the remote refuses, makes init fail, with --json too, instead of
// promising a retry.
func TestInitFailsWhenNoLaterSyncCanPublish(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"a commit made by hand", gitsync.ErrLocalCommits},
		{"a push the remote refuses", gitsync.ErrRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			if tc.want == gitsync.ErrLocalCommits {
				gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "by hand")
			} else {
				hook := filepath.Join(remote, "hooks", "pre-receive")
				if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'protected branch' >&2\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "waits for the remote"); err != nil {
					t.Fatal(err)
				}
			}
			fleetFileAged(t, dir)
			if _, _, err := exec(t, "init", "--dir", dir, remote); !errors.Is(err, tc.want) {
				t.Fatalf("init: %v, want %v", err, tc.want)
			}
			stdout, _, err := exec(t, "init", "--json", "--dir", dir, remote)
			var out struct {
				SyncError string `json:"sync_error"`
			}
			if !errors.Is(err, tc.want) || json.Unmarshal([]byte(stdout), &out) != nil || out.SyncError == "" {
				t.Fatalf("init --json: %v, output %q; want %v and sync_error", err, stdout, tc.want)
			}
		})
	}
}

// Re-filing is bounded by the sync's --timeout, whether git stalls or a rename
// keeps failing, and a sync that runs out of time while re-filing says so,
// rather than that the clone has no upstream.
func TestASyncThatRunsOutOfTimeWhileRefilingSaysSo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script in place of git")
	}
	for _, tc := range []struct{ name, stall string }{
		{"git stalls reading a published copy", "ls-tree"},
		{"git stalls saying whether a file is tracked", "ls-files"},
		{"a rename keeps failing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			recordWithout(t, dir, "--note", "written under no salt")
			if tc.stall == "" {
				// A rename onto a directory that holds a file fails every time, as
				// one Windows refuses while another process has the file open does.
				if err := os.MkdirAll(filepath.Join(dir, ".git", "fleetd-pre-init", "refiled.json", "inside"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				real, err := osexec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = " + tc.stall + " ] && exec sleep 30; done\nexec " + real + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			fleetFileAged(t, dir)
			start := time.Now()
			_, _, err := exec(t, "sync", "--dir", dir, "--timeout", "1s")
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Errorf("a sync with a 1s timeout took %v", elapsed)
			}
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gitsync.ErrNoUpstream) {
				t.Fatalf("sync: %v, want the deadline", err)
			}
		})
	}
}

// An init whose sync runs out of time has still set the journal up, and the next
// sync retries the rest: it says so and succeeds, rather than reporting commits
// that are not there.
func TestInitWhoseSyncRanOutOfTimeSaysTheNextOneRetries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script in place of git")
	}
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	real, err := osexec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = merge-base ] && exec sleep 30; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fleetFileAged(t, dir)
	stdout, _, err := exec(t, "init", "--dir", dir, "--timeout", "4s", remote)
	if err != nil || !strings.Contains(stdout, "the next one retries") || !strings.Contains(stdout, context.DeadlineExceeded.Error()) {
		t.Fatalf("init whose sync ran out of time: %v, %q; want success, saying the next sync retries", err, stdout)
	}
}

// init that makes the fleet's salt up says so, rather than that it was given one.
func TestInitSaysWhenItMadeTheSaltUp(t *testing.T) {
	remote := emptyJournalRemote(t)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, filepath.Dir(seed), "clone", "--quiet", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("journal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "start")
	gitIn(t, seed, "push", "--quiet", "-u", "origin", "main")
	stdout, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), remote)
	if err != nil || !strings.Contains(stdout, "with a new salt for this fleet") || strings.Contains(stdout, "was given") {
		t.Fatalf("init: %v, %q; want it to say the salt is new", err, stdout)
	}
}

// A machine that joins a journal another machine records into has that machine's
// records in its clone as soon as init returns, and nothing to commit.
func TestASecondMachineHasTheOthersRecordsAfterInit(t *testing.T) {
	remote := emptyJournalRemote(t)
	first := filepath.Join(t.TempDir(), "first")
	if _, _, err := exec(t, "init", "--dir", first, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	record := `{"id":"hive:1"}` + "\n"
	if err := os.WriteFile(filepath.Join(first, "host-0123456789abcdef.jsonl"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, first, "add", "host-0123456789abcdef.jsonl")
	gitIn(t, first, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "another machine")
	gitIn(t, first, "push", "--quiet")
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "host-0123456789abcdef.jsonl")); err != nil || string(got) != record {
		t.Fatalf("the other machine's file holds %q (%v), want its records", got, err)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after init:\n%s", out)
	}
}

// On a clone made before the journal had fleetd.json, as a v0.1.0 machine has,
// init puts the journal's fleetd.json in place and files the records written
// under no salt itself, without waiting for another sync, leaving nothing to
// commit.
func TestInitOnAnOldCloneFilesItsRecordsItself(t *testing.T) {
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
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "recorded before init"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote)
	if err != nil || !strings.Contains(stdout, "published 1 record") {
		t.Fatalf("init: %v, %q; want it to publish the record", err, stdout)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "recorded before init"); !ok {
		t.Fatal("after init, the record written before it is on no remote file")
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after init:\n%s", out)
	}
}

// init --salt that contradicts the journal's salt is refused, and sets nothing up.
func TestInitRefusesASaltThatContradictsTheJournalsAndSetsNothingUp(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "the-fleets", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "another", remote); !errors.Is(err, gitsync.ErrSaltMismatch) || !strings.Contains(err.Error(), "--salt") {
		t.Fatalf("init --salt another: %v, want a salt mismatch naming --salt", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init set the journal up anyway: %v", err)
	}
}

// A clone whose fleetd.json holds another salt than the journal's, edited by
// hand or left by an earlier setup, gets the journal's from init, as a new clone
// does, and the records written under the other salt are published under the
// fleet's.
func TestInitOnACloneReplacesAFleetFileWithAnotherSalt(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "the-fleets", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "another"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "written under the other salt"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "init", "--dir", dir, remote)
	if err != nil {
		t.Fatal(err)
	}
	if fleet, ok, err := gitsync.ReadFleet(dir); err != nil || !ok || fleet.Salt != "the-fleets" {
		t.Fatalf("after init the clone's fleetd.json is %+v (%v, %v), want the journal's salt", fleet, ok, err)
	}
	if _, ok := remoteHostRecords(t, remote, "the-fleets", "written under the other salt"); !ok {
		t.Fatalf("after init, the record written under the replaced salt is on no remote file; init said %q", stdout)
	}
	fleetFileAged(t, dir)
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteHostRecords(t, remote, "the-fleets", "written under the other salt"); !ok {
		t.Fatal("the record written under the other salt is not published under the fleet's")
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after the sync:\n%s", out)
	}
}

// A clone with no sync noted, such as one fleetd v0.1.0 synced, is not said
// never to have synced: where says no sync is noted.
func TestWhereOnACloneWithNoSyncNotedSaysSo(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "work"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "where", "--dir", dir)
	if err != nil || !strings.Contains(stdout, "no sync of this journal is noted") || strings.Contains(stdout, "has not synced") {
		t.Fatalf("where: %v\n%s", err, stdout)
	}
}

// init that starts an empty journal with the salt it was given says so, rather
// than that the salt is new.
func TestInitStartingAJournalWithAGivenSaltSaysSo(t *testing.T) {
	remote := emptyJournalRemote(t)
	stdout, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "journal"), "--salt", "s", remote)
	if err != nil || !strings.Contains(stdout, "with the salt this machine was given") || strings.Contains(stdout, "new salt") {
		t.Fatalf("init: %v, %q; want it to say the salt was given", err, stdout)
	}
}

// An old clone, made before the journal had fleetd.json, holding a copy of the
// journal's fleetd.json put there by hand: init puts it in the index too, so no
// sync keeps it as this machine's own change, and a later salt change reaches it.
func TestInitAdoptsAnIdenticalFleetFileGitDoesNotTrack(t *testing.T) {
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
	first := filepath.Join(t.TempDir(), "first")
	if _, _, err := exec(t, "init", "--dir", first, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	fleet, err := os.ReadFile(filepath.Join(first, gitsync.FleetFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), fleet, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "sync", "--dir", dir)
	if err != nil || strings.Contains(stdout, "kept") {
		t.Fatalf("sync after init: %v\n%s", err, stdout)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after the sync:\n%s", out)
	}
}

// A clone checked out with CRLF line endings, as git for Windows does, already
// holds the journal's fleetd.json: init leaves it as it is, rather than writing
// it again and waiting for it to settle.
func TestInitOnACRLFCloneLeavesItsFleetFileAlone(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "-c", "core.autocrlf=true", "clone", "--quiet", "--config", "core.autocrlf=true", remote, dir)
	path := filepath.Join(dir, gitsync.FleetFile)
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "\r\n") {
		t.Fatalf("fleetd.json was not checked out with CRLF: %q (%v)", data, err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("init wrote fleetd.json again: modified %v (%v), want %v", info.ModTime(), err, old)
	}
}

// A clone whose git converts no line endings holds the journal's fleetd.json with
// CRLF endings, saved by a Windows editor: git sees it changed, so init checks
// the journal's out over it. Left alone, it would keep every later change to the
// journal's fleetd.json from reaching this machine.
func TestInitChecksOutAFleetFileGitSeesAsChanged(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "-c", "core.autocrlf=false", "clone", "--quiet", "--config", "core.autocrlf=false", remote, dir)
	path := filepath.Join(dir, gitsync.FleetFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("after init fleetd.json holds %q (%v), want git's copy %q", got, err, data)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after init:\n%s", out)
	}
}

// init on a clone notes the salt of a fleetd.json it replaces before replacing
// it. When the note cannot be written, init fails and leaves the file as it was,
// so that running it again loses nothing.
func TestInitOnACloneKeepsAFleetFileWhoseSaltItCannotNote(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "the-fleets", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "another"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The note cannot be written: a directory stands where it goes.
	obstacle := filepath.Join(dir, ".git", "fleetd-past-salts")
	if err := os.MkdirAll(obstacle, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil {
		t.Fatal("init replaced a fleetd.json whose salt it could not note")
	}
	if fleet, ok, err := gitsync.ReadFleet(dir); err != nil || !ok || fleet.Salt != "another" {
		t.Fatalf("after the failed init fleetd.json is %+v (%v, %v), want it as it was", fleet, ok, err)
	}
	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if past := gitsync.PastSalts(filepath.Join(dir, ".git")); !slices.Contains(past, "another") {
		t.Fatalf("init run again did not note the replaced salt: %q", past)
	}
}

// rotateSalt changes the journal's fleetd.json to one with salt, as a person
// changing the fleet's salt from another machine would.
func rotateSalt(t *testing.T, remote, salt string) {
	t.Helper()
	editor := filepath.Join(t.TempDir(), "editor")
	gitIn(t, filepath.Dir(editor), "clone", "--quiet", remote, editor)
	if err := os.WriteFile(filepath.Join(editor, gitsync.FleetFile), []byte(`{"salt": "`+salt+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, editor, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-am", "a new salt")
	gitIn(t, editor, "push", "--quiet")
}

// init on a clone writes the journal's fleetd.json before it puts it in git's
// index. An init stopped between the two, by a locked index say, leaves the
// journal's salt in place for every record from then on, and init run again
// finishes. Put in the index first, the journal's copy would sit beside a work
// tree file no sync replaces, since a sync keeps a file that differs from git's.
func TestAnInitWhoseIndexUpdateFailsLeavesTheJournalsSaltInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script in place of git")
	}
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "A", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	rotateSalt(t, remote, "B")
	real, err := osexec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *update-index*fleetd.json*) echo 'fatal: Unable to create index.lock: File exists' >&2; exit 128;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil {
		t.Fatal("the index update did not fail")
	}
	t.Setenv("PATH", path)
	if fleet, ok, err := gitsync.ReadFleet(dir); err != nil || !ok || fleet.Salt != "B" {
		t.Fatalf("after an init whose index update failed, fleetd.json is %+v (%v, %v); want the journal's salt B", fleet, ok, err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if out, _ := osexec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status after init ran again:\n%s", out)
	}
}

// A clone on a branch with no commit yet, made with `git checkout --orphan`, was
// not cloned from the empty repository: its other branches may hold commits of
// their own. init refuses it, as any clone without an upstream, and moves none of
// its branches.
func TestInitRefusesACloneOnAnOrphanBranch(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "notes.txt")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "not pushed")
	mine, err := osexec.Command("git", "-C", dir, "rev-parse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "checkout", "--quiet", "--orphan", "scratch")
	// Its git directory holds commits: init must not advise deleting it.
	if _, _, err := exec(t, "init", "--dir", dir, remote); !errors.Is(err, gitsync.ErrNoUpstream) || strings.Contains(err.Error(), "delete its .git") {
		t.Fatalf("init on a clone on an orphan branch: %v, want ErrNoUpstream, not advice to delete its .git directory", err)
	}
	if now, err := osexec.Command("git", "-C", dir, "rev-parse", "main").Output(); err != nil || string(now) != string(mine) {
		t.Fatalf("init moved main from %s to %s (%v)", mine, now, err)
	}
}

// A clone with no commit and no branch, such as a plain clone of the journal
// repository made while it was empty, or a git init given the journal as its
// origin, has nothing to follow. init refuses it, and its advice works: with the
// .git directory gone, init sets the directory up as a new one, keeping its files.
func TestInitOnACloneWithNoCommitSaysHowToSetItUp(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, remote, dir string)
	}{
		{"a plain clone of the empty journal", func(t *testing.T, remote, dir string) {
			gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
		}},
		{"git init with the journal as its origin", func(t *testing.T, remote, dir string) {
			gitIn(t, filepath.Dir(dir), "init", "--quiet", "--initial-branch=main", dir)
			gitIn(t, dir, "remote", "add", "origin", remote)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			c.make(t, remote, dir)
			if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "written before init"); err != nil {
				t.Fatal(err)
			}
			_, _, err := exec(t, "init", "--dir", dir, remote)
			if !errors.Is(err, gitsync.ErrNoUpstream) || !strings.Contains(err.Error(), "no commit") || !strings.Contains(err.Error(), ".git") {
				t.Fatalf("init on a clone with no commit: %v; want it refused, saying to delete its .git directory", err)
			}
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			stdout, _, err := exec(t, "init", "--dir", dir, remote)
			if err != nil {
				t.Fatalf("init after following the advice: %v\n%s", err, stdout)
			}
			fleet, ok, err := gitsync.ReadFleet(dir)
			if err != nil || !ok {
				t.Fatalf("after init the journal's fleetd.json is %+v (%v, %v)", fleet, ok, err)
			}
			if _, ok := remoteHostRecords(t, remote, fleet.Salt, "written before init"); !ok {
				t.Fatalf("the record written before init is on no remote file; init said %q", stdout)
			}
		})
	}
}

// A clone whose fleetd.json is a symbolic link, even to a file holding the
// journal's own content, gets a regular file in its place: fleetd reads
// fleetd.json only as one. A link's salt is never noted.
func TestInitReplacesAFleetFileThatIsASymbolicLink(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	path := filepath.Join(dir, gitsync.FleetFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("after init fleetd.json is %v (%v), want a regular file", info.Mode(), err)
	}
	if past := gitsync.PastSalts(filepath.Join(dir, ".git")); len(past) != 0 {
		t.Fatalf("init noted %q as past salts for a link to the journal's own content", past)
	}
}

// fleetd.json comes from the remote, so any machine able to push can make it
// something other than a file. Records and syncs go on with the salt it last
// held, and a sync brings in the fixed file.
func TestAFleetFileThatBecameADirectoryDoesNotStopRecordingOrSyncing(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(t.TempDir(), "editor")
	gitIn(t, filepath.Dir(editor), "clone", "--quiet", remote, editor)
	good, err := os.ReadFile(filepath.Join(editor, gitsync.FleetFile))
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, editor, "rm", "--quiet", gitsync.FleetFile)
	if err := os.MkdirAll(filepath.Join(editor, gitsync.FleetFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(editor, gitsync.FleetFile, "x"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, editor, "add", "-A")
	gitIn(t, editor, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "a directory")
	gitIn(t, editor, "push", "--quiet")
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, gitsync.FleetFile)); err != nil || !info.IsDir() {
		t.Fatalf("the sync did not bring in the directory: %v", err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "while fleetd.json is a directory"); err != nil {
		t.Fatalf("record with fleetd.json a directory: %v", err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatalf("sync with fleetd.json a directory: %v", err)
	}
	gitIn(t, editor, "pull", "--quiet", "--ff-only")
	gitIn(t, editor, "rm", "--quiet", "-r", gitsync.FleetFile)
	if err := os.WriteFile(filepath.Join(editor, gitsync.FleetFile), good, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, editor, "add", gitsync.FleetFile)
	gitIn(t, editor, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "a file again")
	gitIn(t, editor, "push", "--quiet")
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if fleet, ok, err := gitsync.ReadFleet(dir); err != nil || !ok || fleet.Salt != "s" {
		t.Fatalf("after the fix was pushed, a sync left fleetd.json %+v (%v, %v)", fleet, ok, err)
	}
	if _, ok := remoteHostRecords(t, remote, "s", "while fleetd.json is a directory"); !ok {
		t.Fatal("the record written while fleetd.json was a directory was not published under the fleet's salt")
	}
}

// init on a clone whose fleetd.json is a directory says so, and what to do,
// rather than failing on a rename it cannot make.
func TestInitOnACloneWhoseFleetFileIsADirectorySaysSo(t *testing.T) {
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	path := filepath.Join(dir, gitsync.FleetFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err == nil || !strings.Contains(err.Error(), "is a directory; remove it") {
		t.Fatalf("init with fleetd.json a directory: %v; want it to say so, and to remove it", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "init", "--dir", dir, remote); err != nil {
		t.Fatalf("init once the directory was removed: %v", err)
	}
}
