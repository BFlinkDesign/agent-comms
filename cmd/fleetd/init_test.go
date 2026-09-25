package main

import (
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func TestASaltThatContradictsTheJournalsIsRefusedAndNothingIsWritten(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, gitsync.FleetFile), []byte(`{"salt": "the-fleets"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "a-stale-one")
	for _, args := range [][]string{
		{"record", "--dir", dir, "--type", "note", "--note", "should not be written"},
		{"sync", "--dir", dir},
		{"host", "--dir", dir},
	} {
		if _, _, err := exec(t, args...); !errors.Is(err, gitsync.ErrSaltMismatch) || !strings.Contains(err.Error(), "FLEET_SALT") {
			t.Fatalf("%s: err = %v, want a salt mismatch naming FLEET_SALT", args[0], err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the journal directory holds %d entries, want only fleetd.json", len(entries))
	}
	// Repeating the journal's salt is fine.
	t.Setenv("FLEET_SALT", "the-fleets")
	if _, _, err := exec(t, "host", "--dir", dir); err != nil {
		t.Fatal(err)
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
	if !strings.Contains(stdout, "kept 1 record") {
		t.Fatalf("stdout = %q", stdout)
	}
	if stdout, _, err := exec(t, "sync", "--dir", dir); err != nil || !strings.Contains(stdout, "published 1 record") {
		t.Fatalf("sync: %q, %v", stdout, err)
	}
	if stdout, _, _ := exec(t, "where", "--dir", dir); !strings.Contains(stdout, "written before the journal existed") {
		t.Fatalf("where lost the record: %q", stdout)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
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
