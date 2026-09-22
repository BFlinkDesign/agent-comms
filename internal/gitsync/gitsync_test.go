package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run the real git binary against real repositories: one bare
// repository standing in for the journal remote, and one clone per machine. Each
// case below was a failure an independent review reproduced against the first
// version of this package.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// A clean global configuration, so the machine running the tests cannot
	// change what is tested. Tests that care about hostile settings add them.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fleet creates a remote and n machines' clones of it. Each clone's root is that
// machine's journal directory.
func fleet(t *testing.T, n int) (remote string, machines []string) {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", "--quiet", remote, seed)
	identify(t, seed)
	write(t, filepath.Join(seed, "README.md"), "one file per machine\n")
	run(t, seed, "add", ".")
	run(t, seed, "commit", "--quiet", "-m", "start the journal")
	run(t, seed, "push", "--quiet", "-u", "origin", "main")
	for i := range n {
		dir := filepath.Join(root, fmt.Sprintf("machine-%c", 'a'+i))
		run(t, root, "clone", "--quiet", remote, dir)
		identify(t, dir)
		machines = append(machines, dir)
	}
	return remote, machines
}

func identify(t *testing.T, dir string) {
	run(t, dir, "config", "user.name", "fleet test")
	run(t, dir, "config", "user.email", "fleet@example.invalid")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func options(clone, host string) Options {
	return Options{Dir: clone, File: filepath.Join(clone, host+".jsonl"), Message: "journal: " + host}
}

func mustSync(t *testing.T, o Options) Result {
	t.Helper()
	res, err := Sync(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// remoteFile returns a file as the remote's main branch has it.
func remoteFile(t *testing.T, remote, name string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", remote, "show", "main:"+name)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func TestEachMachinePublishesItsOwnFileAndReceivesTheOthers(t *testing.T) {
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]

	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`, `{"id":"hive:2"}`)
	if res := mustSync(t, options(a, "host-a")); res.Published != 2 || res.Received != 0 || res.Attempts != 1 {
		t.Fatalf("machine a: %+v", res)
	}
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:3"}`)
	if res := mustSync(t, options(b, "host-b")); res.Published != 1 || res.Received != 1 {
		t.Fatalf("machine b should publish 1 and receive a's commit: %+v", res)
	}
	if res := mustSync(t, options(a, "host-a")); res.Published != 0 || res.Received != 1 {
		t.Fatalf("machine a should receive b's commit and publish nothing: %+v", res)
	}
	for _, clone := range []string{a, b} {
		for _, host := range []string{"host-a", "host-b"} {
			got, err := os.ReadFile(filepath.Join(clone, host+".jsonl"))
			if err != nil || string(got) != remoteFile(t, remote, host+".jsonl") {
				t.Errorf("%s has %q for %s, the remote has %q (%v)", filepath.Base(clone), got, host, remoteFile(t, remote, host+".jsonl"), err)
			}
		}
	}
	if status := run(t, a, "status", "--porcelain"); status != "" {
		t.Errorf("after syncing, the clone should be clean, status is %q", status)
	}
}

func TestOnlyThisHostsFileIsEverPublished(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	appendLines(t, filepath.Join(a, "host-z.jsonl"), `{"id":"hive:not-mine"}`)
	write(t, filepath.Join(a, "README.md"), "edited locally\n")

	mustSync(t, options(a, "host-a"))

	changed := run(t, a, "--git-dir", remote, "show", "--name-only", "--format=", "main")
	if changed != "host-a.jsonl" {
		t.Fatalf("the published commit changed %q; it may change only this host's file", changed)
	}
	if remoteFile(t, remote, "README.md") != "one file per machine\n" || remoteFile(t, remote, "host-z.jsonl") != "" {
		t.Fatal("something other than this host's file reached the remote")
	}
}

func TestLocalCommitsAreNeverPushedAndNeverDiscarded(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	write(t, filepath.Join(a, "WIP.txt"), "unfinished\n")
	run(t, a, "add", "WIP.txt")
	run(t, a, "commit", "--quiet", "-m", "unfinished local work")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)

	_, err := Sync(context.Background(), options(a, "host-a"))
	if !errors.Is(err, ErrLocalCommits) {
		t.Fatalf("expected ErrLocalCommits, got %v", err)
	}
	if remoteFile(t, remote, "WIP.txt") != "" || remoteFile(t, remote, "host-a.jsonl") != "" {
		t.Fatal("nothing may be pushed from a clone with commits fleetd did not make")
	}
	if run(t, a, "log", "-1", "--format=%s") != "unfinished local work" {
		t.Fatal("the local commit must be left where it was")
	}
}

func TestAJournalDirectoryInsideAnotherRepositoryIsRefused(t *testing.T) {
	_, m := fleet(t, 1)
	sub := filepath.Join(m[0], "channels", "journal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(sub, "host-a.jsonl"), `{"id":"hive:1"}`)
	_, err := Sync(context.Background(), options(sub, "host-a"))
	if !errors.Is(err, ErrNotClone) || !strings.Contains(err.Error(), "directory of its own") {
		t.Fatalf("expected ErrNotClone naming the fix, got %v", err)
	}
}

func TestAPushRejectedByAnotherMachineIsRetriedAndSucceeds(t *testing.T) {
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a"}`)
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b"}`)

	// Machine b pushes between a's fetch and a's first push: the race two
	// machines syncing at once produce.
	raced := false
	o := options(a, "host-a")
	o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		if args[0] == "push" && !raced {
			raced = true
			mustSync(t, options(b, "host-b"))
		}
		return Git(ctx, dir, stdin, args...)
	}
	res := mustSync(t, o)
	if res.Attempts != 2 || res.Published != 1 || res.Received != 1 {
		t.Fatalf("expected one lost race, a retry, and b's commit received: %+v", res)
	}
	if remoteFile(t, remote, "host-a.jsonl") == "" || remoteFile(t, remote, "host-b.jsonl") == "" {
		t.Fatal("both machines' files must be on the remote")
	}
}

func TestTwoMachinesWithTheSameHostIDAreReportedAndNothingIsTouched(t *testing.T) {
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]
	appendLines(t, filepath.Join(a, "host-x.jsonl"), `{"id":"hive:from-a"}`)
	mustSync(t, options(a, "host-x"))
	appendLines(t, filepath.Join(b, "host-x.jsonl"), `{"id":"hive:from-b"}`)

	_, err := Sync(context.Background(), options(b, "host-x"))
	if !errors.Is(err, ErrSameFile) {
		t.Fatalf("expected ErrSameFile, got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(b, "host-x.jsonl"))
	if string(got) != "{\"id\":\"hive:from-b\"}\n" {
		t.Fatalf("machine b's own records must be left exactly as they were, got %q", got)
	}
	if remoteFile(t, remote, "host-x.jsonl") != "{\"id\":\"hive:from-a\"}\n" {
		t.Fatal("the remote must be left exactly as it was")
	}
}

func TestARecordStillBeingWrittenWaitsForTheNextSync(t *testing.T) {
	remote, m := fleet(t, 1)
	path := filepath.Join(m[0], "host-a.jsonl")
	write(t, path, "{\"id\":\"hive:1\"}\n{\"id\":\"hive:2\"")
	if res := mustSync(t, options(m[0], "host-a")); res.Published != 1 {
		t.Fatalf("only the complete record may be published: %+v", res)
	}
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:1\"}\n" {
		t.Fatalf("remote has %q", got)
	}
	appendLines(t, path, "}")
	if res := mustSync(t, options(m[0], "host-a")); res.Published != 1 {
		t.Fatalf("the completed record goes out next time: %+v", res)
	}
}

func TestRecordsWrittenDuringSyncsAreNeverLostAndTheCloneNeverSticks(t *testing.T) {
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]
	path := filepath.Join(a, "host-a.jsonl")

	// A writer appends complete lines continuously, as `fleetd record` from
	// several agents would, while machine a syncs over and over and machine b
	// keeps the remote moving.
	var written atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := fmt.Fprintf(f, "{\"id\":\"hive:%d\"}\n", i); err != nil {
				t.Error(err)
				return
			}
			written.Add(1)
			// Faster than any person or agent records, slow enough that the file
			// stays small: the point is concurrency, not volume.
			time.Sleep(200 * time.Microsecond)
		}
	}()
	for i := range 15 {
		if _, err := Sync(context.Background(), options(a, "host-a")); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("sync %d failed while a writer was running: %v", i, err)
		}
		appendLines(t, filepath.Join(b, "host-b.jsonl"), fmt.Sprintf(`{"id":"b%d"}`, i))
		mustSync(t, options(b, "host-b"))
	}
	close(stop)
	wg.Wait()
	mustSync(t, options(a, "host-a"))

	local, _ := os.ReadFile(path)
	published := remoteFile(t, remote, "host-a.jsonl")
	if string(local) != published {
		t.Fatalf("after a final sync the remote must hold every record written: local %d lines, remote %d lines",
			strings.Count(string(local), "\n"), strings.Count(published, "\n"))
	}
	if int64(strings.Count(published, "\n")) != written.Load() {
		t.Fatalf("wrote %d records, %d reached the remote", written.Load(), strings.Count(published, "\n"))
	}
	if _, err := os.Stat(filepath.Join(a, ".git", "rebase-merge")); err == nil {
		t.Fatal("the clone was left mid-rebase")
	}
}

func TestHooksAndSigningCannotStopOrStallASync(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	// A signer that always fails, and a pre-push hook that always refuses.
	cfg := os.Getenv("GIT_CONFIG_GLOBAL")
	write(t, cfg, "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n")
	hook := filepath.Join(a, ".git", "hooks", "pre-push")
	write(t, hook, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	if res := mustSync(t, options(a, "host-a")); res.Published != 1 {
		t.Fatalf("%+v", res)
	}
	if remoteFile(t, remote, "host-a.jsonl") == "" {
		t.Fatal("the record did not reach the remote")
	}
}

func TestAFileGitCallsBinaryIsStillPublished(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	write(t, filepath.Join(a, ".gitattributes"), "*.jsonl binary\n")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	if res := mustSync(t, options(a, "host-a")); res.Published != 1 || remoteFile(t, remote, "host-a.jsonl") == "" {
		t.Fatalf("%+v", res)
	}
}

func TestAHungRemoteIsCutOffNearTheDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the process-group kill this proves is the Unix path; Windows relies on WaitDelay alone")
	}
	_, m := fleet(t, 1)
	a := m[0]
	// An ssh that never answers and leaves a child holding its output open: the
	// case that kept the first version waiting eight times past its deadline.
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	t.Setenv("GIT_SSH_COMMAND", "sleep 30 & sleep 30; true")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Sync(ctx, options(a, "host-a"))
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
	if elapsed > time.Second+waitDelay+2*time.Second {
		t.Fatalf("a hung remote held the sync for %v", elapsed)
	}
	if _, err := os.Stat(filepath.Join(a, ".git", "fleetd-sync.lock")); err == nil {
		t.Fatal("the sync lock was left behind")
	}
}

func TestASecondSyncOfTheSameCloneWaitsItsTurn(t *testing.T) {
	_, m := fleet(t, 1)
	a := m[0]
	lockPath := filepath.Join(a, ".git", "fleetd-sync.lock")
	write(t, lockPath, "12345\n")
	if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	old := time.Now().Add(-staleLock - time.Minute)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), options(a, "host-a")); err != nil {
		t.Fatalf("an abandoned lock must not block forever: %v", err)
	}
}

func TestACloneWithoutAnUpstreamSaysHowToSetOne(t *testing.T) {
	_, m := fleet(t, 1)
	run(t, m[0], "switch", "--quiet", "-c", "local-only")
	_, err := Sync(context.Background(), options(m[0], "h"))
	if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "git push -u") {
		t.Fatalf("expected ErrNoUpstream with a fix, got %v", err)
	}
}

func TestADirectoryThatIsNotACloneIsNamedAsSuch(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	if _, err := Sync(context.Background(), options(dir, "host-a")); !errors.Is(err, ErrNotClone) {
		t.Fatalf("expected ErrNotClone, got %v", err)
	}
}

func TestNothingToPublishStillReceives(t *testing.T) {
	_, m := fleet(t, 2)
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(m[0], "host-a"))
	if res := mustSync(t, options(m[1], "host-b")); res.Published != 0 || res.Received != 1 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(m[1], "host-a.jsonl")); err != nil {
		t.Fatal("machine b did not receive machine a's file")
	}
}
