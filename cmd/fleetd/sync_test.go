package main

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of the journal, end to end: two machines each record what they
// did, sync through one git remote, and either machine can then say which machine
// did what. Real git, real files, the real fleetd commands.

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := osexec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func twoMachines(t *testing.T) (a, b string) {
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

	root := t.TempDir()
	remote := filepath.Join(root, "journal.git")
	gitIn(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	a, b = filepath.Join(root, "cnc-1"), filepath.Join(root, "hpremote")
	for i, dir := range []string{a, b} {
		gitIn(t, root, "clone", "--quiet", remote, dir)
		gitIn(t, dir, "config", "user.name", filepath.Base(dir))
		gitIn(t, dir, "config", "user.email", filepath.Base(dir)+"@example.invalid")
		gitIn(t, dir, "config", "commit.gpgsign", "false")
		if i == 0 {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fleet journal\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, dir, "add", "README.md")
			gitIn(t, dir, "commit", "--quiet", "-m", "start")
			gitIn(t, dir, "push", "--quiet", "-u", "origin", "main")
		} else {
			gitIn(t, dir, "pull", "--quiet")
		}
	}
	return a, b
}

func TestTwoMachinesSyncAndEitherCanSayWhoDidWhat(t *testing.T) {
	a, b := twoMachines(t)
	// Distinct salts stand in for distinct machines: the host id is a salted
	// digest, so each clone gets its own journal file, as two real PCs would.
	steps := [][]string{
		{"record", "--dir", a, "--salt", "machine-a", "--type", "note", "--note", "fixed the auto-merge gate", "--repo", "dev-setup"},
		{"sync", "--dir", a, "--salt", "machine-a"},
		{"record", "--dir", b, "--salt", "machine-b", "--type", "note", "--note", "reviewed PR 89", "--repo", "dev-setup"},
		{"sync", "--dir", b, "--salt", "machine-b"},
		{"sync", "--dir", a, "--salt", "machine-a"},
	}
	for _, args := range steps {
		if _, stderr, err := exec(t, args...); err != nil {
			t.Fatalf("fleetd %s: %v\n%s", strings.Join(args, " "), err, stderr)
		}
	}

	for _, dir := range []string{a, b} {
		stdout, _, err := exec(t, "where", "--dir", dir, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var hosts []map[string]any
		if err := json.Unmarshal([]byte(stdout), &hosts); err != nil {
			t.Fatalf("where --json: %v\n%s", err, stdout)
		}
		notes := map[string]bool{}
		for _, h := range hosts {
			notes[h["last_note"].(string)] = true
		}
		if len(hosts) != 2 || !notes["fixed the auto-merge gate"] || !notes["reviewed PR 89"] {
			t.Fatalf("%s should see both machines' work after syncing, got %s", filepath.Base(dir), stdout)
		}
	}
}

func TestSyncReportsWhatItDidAsJSON(t *testing.T) {
	a, _ := twoMachines(t)
	if _, _, err := exec(t, "record", "--dir", a, "--salt", "s", "--type", "note", "--note", "one"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := exec(t, "sync", "--dir", a, "--salt", "s", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Published int    `json:"published"`
		Received  int    `json:"received"`
		Head      string `json:"head"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if res.Published != 1 || len(res.Head) != 40 {
		t.Fatalf("%+v", res)
	}
}

func TestSyncOutsideACloneExplainsItself(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	_, _, err := exec(t, "sync", "--dir", t.TempDir(), "--salt", "s")
	if err == nil || !strings.Contains(err.Error(), "not the root of a git clone") {
		t.Fatalf("expected a not-a-clone error, got %v", err)
	}
}

func TestSyncNamesAFileItLeftAloneAndHowToTakeTheRemotesCopy(t *testing.T) {
	a, b := twoMachines(t)
	if err := os.WriteFile(filepath.Join(a, "README.md"), []byte("edited on cnc-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "README.md"), []byte("edited on hpremote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, b, "commit", "--quiet", "--all", "-m", "by hand")
	gitIn(t, b, "push", "--quiet")

	stdout, stderr, err := exec(t, "sync", "--dir", a, "--salt", "s")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "kept this machine's copy of README.md") ||
		!strings.Contains(stdout, "checkout '@{upstream}' -- <file>") {
		t.Fatalf("sync should name the file it kept and the way to take the remote's copy:\n%s", stdout)
	}
	got, _ := os.ReadFile(filepath.Join(a, "README.md"))
	if string(got) != "edited on cnc-1\n" {
		t.Fatalf("the local edit was overwritten: %q", got)
	}
}
