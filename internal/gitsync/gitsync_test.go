package gitsync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the real git binary against real repositories: one bare
// repository standing in for the journal remote, and one clone per machine.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Isolate from the machine's own git configuration: a global hook, signing
	// requirement or default branch name must not change what is being tested.
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// fleet creates a remote and n machines' clones of it, each with a journal/ dir.
func fleet(t *testing.T, n int) (remote string, machines []string) {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)

	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "--quiet", remote, seed)
	identify(t, seed, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(seed, "journal", "README.md"), "one file per machine\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "--quiet", "-m", "start the journal")
	git(t, seed, "push", "--quiet", "-u", "origin", "main")

	for i := range n {
		dir := filepath.Join(root, "machine"+string(rune('a'+i)))
		git(t, root, "clone", "--quiet", remote, dir)
		identify(t, dir, "machine"+string(rune('a'+i)))
		machines = append(machines, dir)
	}
	return remote, machines
}

func identify(t *testing.T, dir, name string) {
	git(t, dir, "config", "user.name", name)
	git(t, dir, "config", "user.email", name+"@example.invalid")
	git(t, dir, "config", "commit.gpgsign", "false")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func syncMachine(t *testing.T, clone, host string, run Runner) Result {
	t.Helper()
	res, err := Sync(context.Background(), Options{
		Dir:     filepath.Join(clone, "journal"),
		File:    filepath.Join(clone, "journal", host+".jsonl"),
		Message: "journal: " + host,
		Run:     run,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEachMachinePublishesItsOwnFileAndReceivesTheOthers(t *testing.T) {
	_, m := fleet(t, 2)
	a, b := m[0], m[1]

	appendLine(t, filepath.Join(a, "journal", "host-a.jsonl"), `{"id":"hive:1"}`)
	appendLine(t, filepath.Join(a, "journal", "host-a.jsonl"), `{"id":"hive:2"}`)
	if res := syncMachine(t, a, "host-a", nil); res.Published != 2 || res.Attempts != 1 {
		t.Fatalf("machine a: %+v", res)
	}

	appendLine(t, filepath.Join(b, "journal", "host-b.jsonl"), `{"id":"hive:3"}`)
	res := syncMachine(t, b, "host-b", nil)
	if res.Published != 1 || res.Received != 1 {
		t.Fatalf("machine b should publish 1 and receive a's commit: %+v", res)
	}

	res = syncMachine(t, a, "host-a", nil)
	if res.Published != 0 || res.Received != 1 {
		t.Fatalf("machine a should receive b's commit and publish nothing: %+v", res)
	}
	for _, clone := range []string{a, b} {
		for _, host := range []string{"host-a", "host-b"} {
			if _, err := os.Stat(filepath.Join(clone, "journal", host+".jsonl")); err != nil {
				t.Errorf("%s is missing %s after sync: %v", clone, host, err)
			}
		}
	}
	if git(t, a, "rev-parse", "HEAD") != git(t, b, "rev-parse", "HEAD") {
		t.Error("the two machines ended on different commits")
	}
}

func TestOnlyThisHostsFileIsEverCommitted(t *testing.T) {
	_, m := fleet(t, 1)
	a := m[0]
	appendLine(t, filepath.Join(a, "journal", "host-a.jsonl"), `{"id":"hive:1"}`)
	// Somebody else's file, and an unrelated change, sitting in the work tree.
	appendLine(t, filepath.Join(a, "journal", "host-z.jsonl"), `{"id":"hive:9"}`)
	write(t, filepath.Join(a, "journal", "README.md"), "edited locally\n")

	syncMachine(t, a, "host-a", nil)

	files := git(t, a, "show", "--name-only", "--format=", "HEAD")
	if files != "journal/host-a.jsonl" {
		t.Fatalf("the sync commit touched %q; it may only touch this host's file", files)
	}
	if status := git(t, a, "status", "--porcelain"); !strings.Contains(status, "host-z.jsonl") || !strings.Contains(status, "README.md") {
		t.Fatalf("other local changes must be left alone, status is %q", status)
	}
}

func TestAPushRejectedByAnotherMachineIsRetriedAndSucceeds(t *testing.T) {
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	appendLine(t, filepath.Join(b, "journal", "host-b.jsonl"), `{"id":"hive:b"}`)
	appendLine(t, filepath.Join(a, "journal", "host-a.jsonl"), `{"id":"hive:a"}`)

	// Machine b pushes in the gap between a's fetch and a's first push, which is
	// exactly the race two machines syncing at once produce.
	raced := false
	run := func(ctx context.Context, dir string, args ...string) (string, error) {
		if args[0] == "push" && !raced {
			raced = true
			syncMachine(t, b, "host-b", nil)
		}
		return Git(ctx, dir, args...)
	}
	res := syncMachine(t, a, "host-a", run)
	if res.Attempts != 2 || res.Published != 1 {
		t.Fatalf("expected one rejected push and a successful retry: %+v", res)
	}
	if got := git(t, a, "log", "--format=%s", "-2"); !strings.Contains(got, "journal: host-a") || !strings.Contains(got, "journal: host-b") {
		t.Fatalf("history after the retry: %q", got)
	}
}

func TestTwoMachinesWithTheSameHostIDAreReportedNotMerged(t *testing.T) {
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	appendLine(t, filepath.Join(a, "journal", "host-x.jsonl"), `{"id":"hive:from-a"}`)
	syncMachine(t, a, "host-x", nil)
	appendLine(t, filepath.Join(b, "journal", "host-x.jsonl"), `{"id":"hive:from-b"}`)

	_, err := Sync(context.Background(), Options{
		Dir: filepath.Join(b, "journal"), File: filepath.Join(b, "journal", "host-x.jsonl"), Message: "journal: host-x",
	})
	if !errors.Is(err, ErrSameFile) {
		t.Fatalf("expected ErrSameFile, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(b, ".git", "rebase-merge")); statErr == nil {
		t.Fatal("the clone was left in the middle of a rebase")
	}
}

func TestADirectoryThatIsNotACloneIsNamedAsSuch(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	_, err := Sync(context.Background(), Options{Dir: dir, File: filepath.Join(dir, "host-a.jsonl"), Message: "m"})
	if !errors.Is(err, ErrNotClone) {
		t.Fatalf("expected ErrNotClone, got %v", err)
	}
}

func TestACloneWithoutAnUpstreamSaysHowToSetOne(t *testing.T) {
	_, m := fleet(t, 1)
	git(t, m[0], "switch", "--quiet", "-c", "local-only")
	_, err := Sync(context.Background(), Options{Dir: filepath.Join(m[0], "journal"), File: filepath.Join(m[0], "journal", "h.jsonl"), Message: "m"})
	if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "git push -u") {
		t.Fatalf("expected ErrNoUpstream with a fix, got %v", err)
	}
}

func TestNothingToPublishStillReceives(t *testing.T) {
	_, m := fleet(t, 2)
	appendLine(t, filepath.Join(m[0], "journal", "host-a.jsonl"), `{"id":"hive:1"}`)
	syncMachine(t, m[0], "host-a", nil)
	res := syncMachine(t, m[1], "host-b", nil) // host-b has never written anything
	if res.Published != 0 || res.Received != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestAHungRemoteIsBoundedByTheContext(t *testing.T) {
	_, m := fleet(t, 1)
	appendLine(t, filepath.Join(m[0], "journal", "host-a.jsonl"), `{"id":"hive:1"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	run := func(ctx context.Context, dir string, args ...string) (string, error) {
		if args[0] == "fetch" {
			<-ctx.Done() // a remote that never answers
			return "", ctx.Err()
		}
		return Git(ctx, dir, args...)
	}
	start := time.Now()
	_, err := Sync(ctx, Options{Dir: filepath.Join(m[0], "journal"), File: filepath.Join(m[0], "journal", "host-a.jsonl"), Message: "m", Run: run})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a hung remote held the sync for %v", elapsed)
	}
}

func TestAddedLinesSumsNumstat(t *testing.T) {
	if got := addedLines("2\t0\tjournal/a.jsonl\n3\t1\tjournal/b.jsonl\n"); got != 5 {
		t.Fatalf("got %d", got)
	}
	if got := addedLines(""); got != 0 {
		t.Fatalf("got %d", got)
	}
}
