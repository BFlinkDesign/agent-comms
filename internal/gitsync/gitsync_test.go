package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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

// TestMain gives every test one clean global git configuration, so that the
// machine running the tests cannot change what is tested. It is set once, for the
// whole package, so that tests can run in parallel; one that cares about hostile
// settings sets them itself, and so runs on its own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gitsync-test")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	// Tests running in parallel share that configuration: one that wrote it
	// would change what git does in every other.
	if data, err := os.ReadFile(cfg); err != nil || len(data) != 0 {
		fmt.Fprintf(os.Stderr, "a test changed the global git configuration every test shares: %q (%v)\n", data, err)
		code = 1
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
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
	return newFleet(t, n)
}

// newFleet is fleet for a subtest, whose parent has called requireGit.
func newFleet(t *testing.T, n int) (remote string, machines []string) {
	t.Helper()
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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
	t.Parallel()
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

// A branch whose name ends in white space beyond ASCII, a no-break space, say,
// is the branch a sync publishes to: read without it, the name is another
// branch's, one origin does not have.
func TestASyncPublishesToABranchWhoseNameEndsInANoBreakSpace(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	branch := "weg\u00a0"
	run(t, a, "push", "--quiet", "origin", "main:refs/heads/"+branch)
	run(t, a, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	run(t, a, "push", "--quiet", "origin", ":refs/heads/main")
	run(t, a, "fetch", "--quiet", "--prune", "origin")
	run(t, a, "branch", "--quiet", "-m", "main", branch)
	run(t, a, "branch", "--quiet", "--set-upstream-to=origin/"+branch, branch)
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
	mustSync(t, options(a, "host-a"))
	// Each name ends in a bar, which run's trimming leaves alone.
	if heads := run(t, a, "--git-dir", remote, "for-each-ref", "--format=%(refname)|", "refs/heads/"); heads != "refs/heads/"+branch+"|" {
		t.Fatalf("the remote has %q, want only %q", heads, "refs/heads/"+branch)
	}
	if got := run(t, a, "--git-dir", remote, "show", branch+":host-a.jsonl"); got != `{"id":"hive:a1"}` {
		t.Fatalf("%q holds %q for host-a", branch, got)
	}
}

// GitDir finds a clone's git directory as git does: .git, or the directory a .git
// file names, absolute or relative to the clone's top.
func TestGitDirFindsTheDirectoryAGitFileNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plain, elsewhere := filepath.Join(root, "plain"), filepath.Join(root, "elsewhere.git")
	for _, d := range []string{filepath.Join(plain, ".git"), elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(plain, ".git", "HEAD"), "ref: refs/heads/main\n")
	for _, c := range []struct {
		name, file string
		want       string
	}{
		{"absolute", "gitdir: " + elsewhere + "\n", elsewhere},
		{"relative, ending in CRLF", "gitdir: ../elsewhere.git\r\n", elsewhere},
		{"naming nothing there", "gitdir: " + filepath.Join(root, "missing") + "\n", ""},
		{"naming a file", "gitdir: " + filepath.Join(root, "plain", ".git", "HEAD") + "\n", ""},
		{"not a .git file", "ref: refs/heads/main\n", ""},
	} {
		dir := filepath.Join(root, strings.ReplaceAll(c.name, " ", "-"))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, ".git"), c.file)
		if got, ok := GitDir(dir); got != c.want || ok != (c.want != "") {
			t.Errorf("%s: GitDir = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
	if got, ok := GitDir(plain); got != filepath.Join(plain, ".git") || !ok {
		t.Errorf("a .git directory: GitDir = %q, %v", got, ok)
	}
	if _, ok := GitDir(root); ok {
		t.Error("GitDir found a git directory where there is none")
	}
}

// GitDir takes a relative path from the journal directory's real path, as git
// takes it: through a link to the journal directory, .. is its real parent.
func TestGitDirTakesARelativePathFromTheRealDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	journal := filepath.Join(root, "real", "journal")
	if err := os.MkdirAll(filepath.Join(root, "real", "journal.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journal, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(journal, ".git"), "gitdir: ../journal.git\n")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(journal, alias); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if got, ok := GitDir(alias); !ok || !SameDir(got, filepath.Join(root, "real", "journal.git")) {
		t.Errorf("through a link: GitDir = %q, %v; want the real journal.git", got, ok)
	}
}

func TestOnlyThisHostsFileIsEverPublished(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]
	// The push names the tip it fetched, so a person's push.useForceIfIncludes,
	// which holds a push that names none to what the clone's reflog has seen,
	// cannot refuse the retry.
	run(t, a, "config", "push.useForceIfIncludes", "true")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a"}`)
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b"}`)

	// Machine b pushes between a's fetch and a's first push: the race two
	// machines syncing at once produce.
	raced := false
	o := options(a, "host-a")
	o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		if slices.Contains(args, "push") && !raced {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	// A signer that always fails, in this machine's own git configuration, and a
	// pre-push hook that always refuses.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	write(t, cfg, "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
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
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	write(t, filepath.Join(a, ".gitattributes"), "*.jsonl binary\n")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	if res := mustSync(t, options(a, "host-a")); res.Published != 1 || remoteFile(t, remote, "host-a.jsonl") == "" {
		t.Fatalf("%+v", res)
	}
}

func TestAHungRemoteIsCutOffNearTheDeadline(t *testing.T) {
	_, m := fleet(t, 1)
	a := m[0]
	// An ssh that never answers and leaves a child holding its output open: the
	// case that kept the first version waiting eight times past its deadline.
	// git runs this through sh on every platform, Git for Windows' bundled sh
	// included. The simple variant makes git run it once, for the connection,
	// with git's own output; otherwise git first probes it with -G and output
	// that goes nowhere. This test proves only the deadline bound, which
	// WaitDelay meets even if git alone is killed;
	// TestCancellingASyncKillsEveryProcessGitStarted is the one that proves the
	// whole tree is killed.
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	t.Setenv("GIT_SSH_VARIANT", "simple")
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
	// A plain timeout says so and nothing more: the git that was killed for it
	// exits unsuccessfully, which is not a second problem.
	if !strings.HasSuffix(err.Error(), ": "+context.DeadlineExceeded.Error()) {
		t.Fatalf("a plain timeout reads as more than one: %v", err)
	}
	if elapsed > time.Second+waitDelay+2*time.Second {
		t.Fatalf("a hung remote held the sync for %v", elapsed)
	}
	if _, err := os.Stat(filepath.Join(a, ".git", "fleetd-sync.lock")); err == nil {
		t.Fatal("the sync lock was left behind")
	}
}

func TestASecondSyncOfTheSameCloneWaitsItsTurn(t *testing.T) {
	t.Parallel()
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

// initAdvice is what a sync tells a clone that is not on the journal's branch.
func initAdvice(dir string) string {
	return "`fleetd init --dir \"" + dir + "\" <journal URL>`"
}

// offBranch is a journal whose clone a has published a1 and fetched b2, which it
// has not taken in: its main is one commit behind origin's. Its caller has called
// requireGit, so that a parallel test can use it.
func offBranch(t *testing.T) (remote, a, b string) {
	t.Helper()
	remote, m := newFleet(t, 2)
	a, b = m[0], m[1]
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b1"}`)
	mustSync(t, options(b, "host-b"))
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
	mustSync(t, options(a, "host-a"))
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b2"}`)
	mustSync(t, options(b, "host-b"))
	run(t, a, "fetch", "--quiet")
	return remote, a, b
}

// A clone a person moved off the journal's branch is told to run fleetd init,
// which puts it back on the journal's branch without touching a file: git's own
// commands to move a branch refuse, or overwrite this machine's records, when the
// branch's files lack them. Each state ends with every record published to main,
// from a work tree that holds every machine's file.
func TestAClonePutOffTheJournalsBranchIsPutBackByInit(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name  string
		leave [][]string
	}{
		{"a branch made by hand", [][]string{{"switch", "--quiet", "-c", "local-only"}}},
		{"a detached HEAD", [][]string{{"checkout", "--quiet", "--detach"}}},
		{"a detached HEAD and no branch, beside a tag and another remote's branch of its name", [][]string{
			{"checkout", "--quiet", "--detach"}, {"branch", "--quiet", "-D", "main"}, {"tag", "main"},
			{"remote", "add", "other", "../remote.git"}, {"fetch", "--quiet", "other"},
		}},
		{"a branch whose upstream was unset", [][]string{{"branch", "--unset-upstream"}}},
		{"a detached HEAD beside a branch whose upstream was unset", [][]string{
			{"branch", "--unset-upstream"}, {"checkout", "--quiet", "--detach"},
		}},
		{"main following a branch of this clone", [][]string{{"branch", "--quiet", "x"}, {"branch", "--quiet", "-u", "x"}}},
		{"main following another remote's branch the clone no longer has", [][]string{
			{"remote", "add", "other", "../remote.git"}, {"fetch", "--quiet", "other"}, {"branch", "--quiet", "-u", "other/main"},
			{"update-ref", "-d", "refs/remotes/other/main"},
		}},
		{"a detached HEAD beside main following another of origin's branches", [][]string{
			{"push", "--quiet", "origin", "main:refs/heads/stray"}, {"fetch", "--quiet"},
			{"checkout", "--quiet", "--detach"}, {"branch", "--quiet", "-u", "origin/stray", "main"},
		}},
		{"an orphan branch with an emptied index", [][]string{
			{"checkout", "--quiet", "--orphan", "scratch"}, {"rm", "-r", "-q", "--cached", "."},
		}},
		{"an orphan branch with an emptied index and main deleted", [][]string{
			{"checkout", "--quiet", "--orphan", "scratch"}, {"rm", "-r", "-q", "--cached", "."}, {"branch", "--quiet", "-D", "main"},
		}},
		{"a detached HEAD beside a branch named mine that follows main", [][]string{
			{"branch", "--quiet", "-m", "main", "mine"}, {"checkout", "--quiet", "--detach"},
		}},
		{"a detached HEAD beside a branch named stray that follows main, which origin has too", [][]string{
			{"push", "--quiet", "origin", "main:refs/heads/stray"}, {"fetch", "--quiet"},
			{"branch", "--quiet", "-m", "main", "stray"}, {"checkout", "--quiet", "--detach"},
		}},
		{"a detached HEAD beside main following two branches at once", [][]string{
			{"config", "--add", "branch.main.merge", "refs/heads/other"}, {"checkout", "--quiet", "--detach"},
		}},
		{"a detached HEAD at origin's tip, with main behind its own", [][]string{
			{"checkout", "--quiet", "--detach", "origin/main"}, {"branch", "--quiet", "-f", "main", "main~1"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, a, _ := offBranch(t)
			for _, args := range c.leave {
				run(t, a, args...)
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
			_, err := Sync(context.Background(), options(a, "host-a"))
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), initAdvice(a)) {
				t.Fatalf("expected ErrNoUpstream saying %s, got %v", initAdvice(a), err)
			}
			res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
			if !res.Reattached || res.Branch != "main" {
				t.Fatalf("init = %+v, want the clone put back on main", res)
			}
			if got := readFile(t, filepath.Join(a, "host-a.jsonl")); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
				t.Fatalf("after init this machine's file holds %q", got)
			}
			mustSync(t, options(a, "host-a"))
			if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
				t.Fatalf("main holds %q for host-a", got)
			}
			if got := readFile(t, filepath.Join(a, "host-b.jsonl")); got != "{\"id\":\"hive:b1\"}\n{\"id\":\"hive:b2\"}\n" {
				t.Fatalf("the work tree holds %q for host-b", got)
			}
			if up := run(t, a, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/main" {
				t.Fatalf("after init the clone follows %s", up)
			}
			if out := run(t, a, "status", "--porcelain", "--untracked-files=no"); out != "" {
				t.Fatalf("git status after the sync:\n%s", out)
			}
		})
	}
}

// A branch that follows another remote's branch would take this machine's records
// where the fleet never looks: sync stops, and says to run init, which puts the
// clone back on origin's.
func TestABranchFollowingAnotherRemotesIsPutBackOnOrigins(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote, a, _ := offBranch(t)
	backup := filepath.Join(t.TempDir(), "backup.git")
	run(t, a, "clone", "--quiet", "--bare", remote, backup)
	run(t, a, "remote", "add", "backup", backup)
	run(t, a, "fetch", "--quiet", "backup")
	run(t, a, "branch", "--quiet", "-u", "backup/main")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
	_, err := Sync(context.Background(), options(a, "host-a"))
	if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), initAdvice(a)) {
		t.Fatalf("expected ErrNoUpstream saying %s, got %v", initAdvice(a), err)
	}
	if got := remoteFile(t, backup, "host-a.jsonl"); strings.Contains(got, "hive:a2") {
		t.Fatalf("the sync published to the other remote: %q", got)
	}
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "main" {
		t.Fatalf("init = %+v, want the clone put back on main", res)
	}
	mustSync(t, options(a, "host-a"))
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
		t.Fatalf("main holds %q for host-a", got)
	}
}

// Branches are looked up by full name: a branch here named origin/main never
// passes for origin's main, and an orphan zz is not taken for a branch zz/a that
// follows one of origin's. Neither reads as a branch the clone no longer has.
func TestABranchIsLookedUpByItsFullName(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name  string
		leave [][]string
	}{
		{"main following a branch here named origin/main", [][]string{
			{"branch", "--quiet", "origin/main"}, {"config", "branch.main.remote", "."},
			{"config", "branch.main.merge", "refs/heads/origin/main"},
		}},
		{"an orphan zz beside zz/a, which follows a branch origin no longer has", [][]string{
			{"push", "--quiet", "origin", "main:refs/heads/gone"}, {"fetch", "--quiet"},
			{"branch", "--quiet", "--track", "zz/a", "origin/gone"}, {"push", "--quiet", "origin", ":refs/heads/gone"},
			{"fetch", "--quiet", "--prune"}, {"checkout", "--quiet", "--orphan", "zz"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, a, _ := offBranch(t)
			for _, args := range c.leave {
				run(t, a, args...)
			}
			_, err := Sync(context.Background(), options(a, "host-a"))
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), initAdvice(a)) {
				t.Fatalf("expected ErrNoUpstream saying %s, got %v", initAdvice(a), err)
			}
			if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "main" {
				t.Fatalf("init = %+v, want the clone put back on main", res)
			}
			mustSync(t, options(a, "host-a"))
		})
	}
}

// A person who checks out an older commit has git rewrite this machine's file to
// that commit's copy, without the records it published since. init puts the clone
// back all the same, and leaves the file as it is: the sync then says the remote
// holds records this copy lacks, which init --reclaim puts back.
func TestAnOlderCommitCheckedOutIsPutBackAndLeftForReclaim(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote, a, _ := offBranch(t)
	run(t, a, "checkout", "--quiet", "HEAD~1")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
	before := readFile(t, filepath.Join(a, "host-a.jsonl"))
	if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrNoUpstream) {
		t.Fatalf("expected ErrNoUpstream, got %v", err)
	}
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached {
		t.Fatalf("init = %+v, want the clone put back", res)
	}
	if got := readFile(t, filepath.Join(a, "host-a.jsonl")); got != before {
		t.Fatalf("init changed this machine's file from %q to %q", before, got)
	}
	if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrSameFile) {
		t.Fatalf("expected ErrSameFile, got %v", err)
	}
}

// A clone whose branch the remote renamed, or deleted on purpose, is told so by
// sync and by init, and nothing is pushed: a push would recreate the branch the
// fleet left. Their own fetches prune the clone's copy of the branch, so this
// holds whether a person pruned it or not, in a clone made with --single-branch
// too, and a tag named like the branch changes nothing. Only a person knows where
// the journal went, so the way on the message gives is init told the branch,
// which puts the clone there, and not on another the clone follows or on the
// remote's default; the next sync publishes there.
func TestABranchTheCloneNoLongerHasIsNamedAsSuch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	renamed := func(to string) func(t *testing.T, remote string) {
		return func(t *testing.T, remote string) {
			run(t, filepath.Dir(remote), "--git-dir", remote, "branch", "-m", "main", to)
		}
	}
	// startedApart is a branch of the remote's that another machine started the
	// journal on, with a history of its own.
	startedApart := func(t *testing.T, remote, branch string) {
		other := filepath.Join(t.TempDir(), "other")
		run(t, filepath.Dir(other), "init", "--quiet", "--initial-branch="+branch, other)
		identify(t, other)
		write(t, filepath.Join(other, "README.md"), "started apart\n")
		run(t, other, "add", ".")
		run(t, other, "commit", "--quiet", "-m", "started apart")
		run(t, other, "push", "--quiet", remote, branch)
	}
	for _, c := range []struct {
		name   string
		single bool
		before [][]string
		change func(t *testing.T, remote string)
		leave  [][]string
		to     string
	}{
		{"renamed on the remote", false, nil, renamed("trunk"), nil, "trunk"},
		{"renamed on the remote, and pruned by hand", false, nil, renamed("trunk"), [][]string{{"fetch", "--quiet", "--prune"}}, "trunk"},
		{"renamed on the remote, beside a tag named like it", false, nil, renamed("trunk"),
			[][]string{{"tag", "main"}, {"fetch", "--quiet", "--prune"}}, "trunk"},
		{"renamed on the remote, in a clone made with --single-branch", true, nil, renamed("trunk"), nil, "trunk"},
		{"renamed on the remote, beside another branch the clone follows", false, [][]string{
			{"push", "--quiet", "origin", "main:refs/heads/stray"}, {"fetch", "--quiet"},
			{"branch", "--quiet", "--track", "stray", "origin/stray"},
		}, renamed("trunk"), nil, "trunk"},
		{"renamed on the remote to a branch that is not its default", false, nil, func(t *testing.T, remote string) {
			startedApart(t, remote, "docs")
			run(t, filepath.Dir(remote), "--git-dir", remote, "branch", "-m", "main", "journal")
			run(t, filepath.Dir(remote), "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/docs")
		}, nil, "journal"},
		{"deleted on purpose, beside a default of its own", false, nil, func(t *testing.T, remote string) {
			// Another machine started the journal on trunk at the same time, and the
			// fleet kept trunk.
			startedApart(t, remote, "trunk")
			run(t, filepath.Dir(remote), "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			run(t, filepath.Dir(remote), "--git-dir", remote, "update-ref", "-d", "refs/heads/main")
		}, nil, "trunk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			a := m[0]
			if c.single {
				a = filepath.Join(t.TempDir(), "a")
				run(t, filepath.Dir(a), "clone", "--quiet", "--single-branch", "--branch", "main", remote, a)
			}
			for _, args := range c.before {
				run(t, a, args...)
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
			mustSync(t, options(a, "host-a"))
			c.change(t, remote)
			for _, args := range c.leave {
				run(t, a, args...)
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
			gone := "main follows origin/main, which this clone no longer has"
			if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), gone) {
				t.Fatalf("sync: %v, want ErrNoUpstream saying the branch is gone", err)
			}
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), gone) {
				t.Fatalf("init: %v, want ErrNoUpstream saying the branch is gone", err)
			}
			if out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "--quiet", "refs/heads/main").Output(); err == nil {
				t.Fatalf("main came back, at %s", out)
			}
			if want := "`fleetd init --dir \"" + a + "\" --branch <branch> <journal URL>`"; !strings.Contains(err.Error(), want) {
				t.Fatalf("init: %v, want it to name %s", err, want)
			}
			if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s", Branch: c.to}); !res.Reattached || res.Branch != c.to {
				t.Fatalf("init = %+v, want the clone put on %s", res, c.to)
			}
			mustSync(t, options(a, "host-a"))
			if got := run(t, a, "--git-dir", remote, "show", c.to+":host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}" {
				t.Fatalf("%s holds %q for host-a", c.to, got)
			}
		})
	}
}

// A branch deleted by mistake, and pushed back from the machine that synced
// last as its own sync says, is taken up again by every other machine's next
// sync, though each had pruned its copy of the branch. The push names origin's
// branch, which the machine's own may not be named after.
func TestABranchPushedBackAfterAMistakenDeletionIsTakenUpAgain(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]
	run(t, b, "branch", "--quiet", "-m", "main", "work")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
	mustSync(t, options(a, "host-a"))
	mustSync(t, options(b, "host-b"))
	run(t, a, "--git-dir", remote, "update-ref", "-d", "refs/heads/main")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
	_, err := Sync(context.Background(), options(a, "host-a"))
	if advice := "`git -C \"" + a + "\" push origin refs/heads/<local>:refs/heads/<branch>`, with main for <local> and " +
		"main for <branch>"; !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), advice) {
		t.Fatalf("sync: %v, want ErrNoUpstream saying %s", err, advice)
	}
	_, err = Sync(context.Background(), options(b, "host-b"))
	if advice := "with work for <local> and main for <branch>"; !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), advice) {
		t.Fatalf("b's sync: %v, want ErrNoUpstream saying %s", err, advice)
	}
	run(t, b, "push", "--quiet", "origin", "refs/heads/work:refs/heads/main")
	mustSync(t, options(a, "host-a"))
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
		t.Fatalf("main holds %q for host-a", got)
	}
}

// A sync on a branch whose copy of a branch the remote deleted is older than
// another branch's here, one a person switched to and synced on, then left,
// names the newer copy to push back: pushing HEAD's would take back the records
// published since, and stop every machine that has them.
func TestASyncNamesTheNewestCopyOfAGoneBranchToPushBack(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
	mustSync(t, options(a, "host-a"))
	run(t, a, "switch", "--quiet", "-c", "mywork", "--track", "origin/main")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
	mustSync(t, options(a, "host-a"))
	newest := run(t, a, "rev-parse", "refs/heads/mywork")
	run(t, a, "symbolic-ref", "HEAD", "refs/heads/main")
	run(t, a, "--git-dir", remote, "update-ref", "-d", "refs/heads/main")
	_, err := Sync(context.Background(), options(a, "host-a"))
	if advice := "with mywork for <local> and main for <branch>"; !errors.Is(err, ErrNoUpstream) ||
		!strings.Contains(err.Error(), advice) || strings.Contains(err.Error(), "push origin HEAD") {
		t.Fatalf("sync: %v, want ErrNoUpstream saying %s, never to push HEAD", err, advice)
	}
	run(t, a, "push", "--quiet", "origin", "refs/heads/mywork:refs/heads/main")
	if got := run(t, a, "--git-dir", remote, "rev-parse", "refs/heads/main"); got != newest {
		t.Fatalf("main is back at %s, want %s, the newest copy", got, newest)
	}
	mustSync(t, options(a, "host-a"))
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
		t.Fatalf("main holds %q for host-a", got)
	}
}

// A branch the remote renames between a sync's fetch and its push is not brought
// back by the push, nor by init's: each pushes onto the tip it fetched and
// nothing else, and then says the branch is gone.
func TestARenameBetweenTheFetchAndThePushRecreatesNothing(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, by := range []string{"sync", "init"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			a := m[0]
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
			renamed := false
			rename := func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
				if !renamed && slices.Contains(args, "push") {
					renamed = true
					run(t, a, "--git-dir", remote, "branch", "-m", "main", "trunk")
				}
				return Git(ctx, dir, stdin, args...)
			}
			var err error
			if by == "sync" {
				o := options(a, "host-a")
				o.Run = rename
				_, err = Sync(context.Background(), o)
			} else {
				_, err = Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s", Run: rename})
			}
			if !renamed || !errors.Is(err, ErrNoUpstream) {
				t.Fatalf("%s: %v, want ErrNoUpstream once the branch was renamed", by, err)
			}
			if heads := run(t, a, "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/"); heads != "refs/heads/trunk" {
				t.Fatalf("the remote has %q, want trunk alone", heads)
			}
		})
	}
}

// fleetd's fetches name their refspec, so a person's fetch.pruneTags does not
// delete the clone's tags, which may be all that holds a commit of theirs.
func TestAPruningFetchLeavesTheClonesTagsAlone(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	run(t, a, "config", "fetch.prune", "true")
	run(t, a, "config", "fetch.pruneTags", "true")
	run(t, a, "tag", "keep")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
	mustSync(t, options(a, "host-a"))
	if tags := run(t, a, "tag"); tags != "keep" {
		t.Fatalf("after the sync the clone's tags are %q", tags)
	}
	mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
	if tags := run(t, a, "tag"); tags != "keep" {
		t.Fatalf("after init the clone's tags are %q", tags)
	}
}

// A sync whose context ends while git is answering a question says the time ran
// out, never what a failure there would otherwise mean: not a clone, no
// upstream, a branch gone from the remote, a detached HEAD, or commits fleetd did
// not make.
func TestASyncThatRunsOutOfTimeSaysSoWhereverItStops(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, at := range []string{"--show-toplevel", "@{u}", "symbolic-ref", "refs/remotes/origin/main", "merge-base", "prepare",
		"detached symbolic-ref", "by hand for-each-ref", "gone symbolic-ref", "gone for-each-ref",
		"gone refs/remotes/origin/main", "gone fetch", "other symbolic-ref"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
			// Off the journal's branch, the time can run out while the sync works out
			// what to say about it; with its branch gone, while it looks for it again.
			switch state, call, _ := strings.Cut(at, " "); state {
			case "detached":
				run(t, m[0], "checkout", "--quiet", "--detach")
				at = call
			case "gone":
				run(t, m[0], "--git-dir", remote, "branch", "-m", "main", "trunk")
				run(t, m[0], "fetch", "--quiet", "--prune")
				at = call
			case "other":
				// main follows another remote's branch, which sync does not take.
				run(t, m[0], "remote", "add", "backup", remote)
				run(t, m[0], "fetch", "--quiet", "backup")
				run(t, m[0], "branch", "--quiet", "-u", "backup/main")
				at = call
			case "by":
				run(t, m[0], "switch", "--quiet", "-c", "local-only")
				at = strings.TrimPrefix(call, "hand ")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o := options(m[0], "host-a")
			o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
				if slices.Contains(args, at) {
					cancel()
				}
				return Git(ctx, dir, stdin, args...)
			}
			o.Prepare = func(context.Context, string) {
				if at == "prepare" {
					cancel()
				}
			}
			_, err := Sync(ctx, o)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the cancellation", err)
			}
			for _, wrong := range []error{ErrNotClone, ErrNoUpstream, ErrLocalCommits} {
				if errors.Is(err, wrong) {
					t.Fatalf("err = %v: the time running out reads as %v", err, wrong)
				}
			}
			if strings.Contains(err.Error(), "detached") || at == "prepare" && !strings.Contains(err.Error(), "preparing the sync") {
				t.Fatalf("err = %v, which does not say where the time ran out", err)
			}
		})
	}
}

// A push the remote refuses for any reason but another machine's push, such as
// branch protection, is reported as refused and not tried again: no later push
// gets past it until a person changes the remote.
func TestAPushTheRemoteRefusesIsReportedAsRefused(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	write(t, hook, "#!/bin/sh\necho 'protected branch' >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	res, err := Sync(context.Background(), options(m[0], "host-a"))
	if !errors.Is(err, ErrRejected) || res.Attempts != 1 {
		t.Fatalf("err = %v after %d attempts, want ErrRejected after one", err, res.Attempts)
	}
}

// A rename Windows keeps refusing is retried for a few seconds, but never past
// the caller's deadline.
func TestARenameRetryStopsWhenTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	write(t, from, "a record\n")
	// Renaming onto a directory that holds a file fails every time.
	if err := os.MkdirAll(filepath.Join(to, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := RenameRetry(ctx, from, to); err == nil {
		t.Fatal("the rename onto a directory worked")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RenameRetry took %v with a 200ms deadline", elapsed)
	}
}

// A repository with no commit yet has no index either; a sync there still says
// what to do rather than failing to fill the index: run init.
func TestARepositoryWithNoCommitIsToldToRunInit(t *testing.T) {
	t.Parallel()
	requireGit(t)
	dir := t.TempDir()
	run(t, dir, "init", "--quiet", "--initial-branch=main")
	_, err := Sync(context.Background(), options(dir, "h"))
	if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), initAdvice(dir)) {
		t.Fatalf("expected ErrNoUpstream saying %s, got %v", initAdvice(dir), err)
	}
}

// A plain clone of the journal repository made while it was still empty has no
// commit. Once the journal has been started, a sync there tells it to run init,
// whether it has fetched since or not, and init puts it on the journal's branch,
// keeping its git directory and the records written meanwhile.
func TestAPlainCloneOfTheEmptyJournalIsPutOnItsBranchByInit(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, fetched := range []bool{false, true} {
		t.Run(map[bool]string{false: "not fetched since", true: "fetched since"}[fetched], func(t *testing.T) {
			t.Parallel()
			remote := newEmptyRemote(t)
			root := filepath.Dir(remote)
			a := filepath.Join(root, "a")
			run(t, root, "clone", "--quiet", remote, a)
			mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(root, "first"), Salt: "s"})
			if fetched {
				run(t, a, "fetch", "--quiet")
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
			_, err := Sync(context.Background(), options(a, "host-a"))
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), initAdvice(a)) {
				t.Fatalf("expected ErrNoUpstream saying %s, got %v", initAdvice(a), err)
			}
			if res := mustInit(t, InitOptions{URL: remote, Dir: a}); !res.Reattached || res.Branch != "main" || res.Salt != "s" {
				t.Fatalf("init = %+v, want the clone put on main with the journal's salt", res)
			}
			setUp(t, a, "s")
			mustSync(t, options(a, "host-a"))
			if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n" {
				t.Fatalf("main holds %q for host-a", got)
			}
		})
	}
}

func TestADirectoryThatIsNotACloneIsNamedAsSuch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	dir := t.TempDir()
	if _, err := Sync(context.Background(), options(dir, "host-a")); !errors.Is(err, ErrNotClone) {
		t.Fatalf("expected ErrNotClone, got %v", err)
	}
}

func TestNothingToPublishStillReceives(t *testing.T) {
	t.Parallel()
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

// commitByHand makes a change on the remote the way a person would, from a clone
// of their own, so a machine's next sync has something other than journal
// records to bring in.
func commitByHand(t *testing.T, clone string, change func(dir string)) {
	t.Helper()
	run(t, clone, "pull", "--quiet", "--ff-only")
	change(clone)
	run(t, clone, "add", "--all")
	run(t, clone, "commit", "--quiet", "-m", "by hand")
	run(t, clone, "push", "--quiet")
}

func TestAnotherIdentitysUnpublishedRecordsAreNeverOverwritten(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	// One machine, two identities: a missing FLEET_SALT is only a warning, so
	// the same machine can record under either.
	second := filepath.Join(a, "host-x2.jsonl")
	appendLines(t, second, `{"id":"hive:x2-published"}`)
	mustSync(t, options(a, "host-x2"))
	appendLines(t, second, `{"id":"hive:x2-unpublished"}`)

	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a"}`)
	res := mustSync(t, options(a, "host-a"))

	got, _ := os.ReadFile(second)
	if string(got) != "{\"id\":\"hive:x2-published\"}\n{\"id\":\"hive:x2-unpublished\"}\n" {
		t.Fatalf("another identity's unpublished record was lost; the file is now %q", got)
	}
	if len(res.Kept) != 0 {
		t.Fatalf("nothing changed on the remote for that file, so nothing should be reported: %+v", res)
	}
	if remoteFile(t, remote, "host-x2.jsonl") != "{\"id\":\"hive:x2-published\"}\n" {
		t.Fatal("a sync as host-a must not publish host-x2's records")
	}
}

func TestAFileChangedHereAndOnTheRemoteIsKeptAndReported(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 2)
	a, admin := m[0], m[1]
	write(t, filepath.Join(a, "README.md"), "edited on this machine\n")
	commitByHand(t, admin, func(dir string) { write(t, filepath.Join(dir, "README.md"), "edited upstream\n") })

	res := mustSync(t, options(a, "host-a"))
	got, _ := os.ReadFile(filepath.Join(a, "README.md"))
	if string(got) != "edited on this machine\n" {
		t.Fatalf("a local edit was overwritten with %q", got)
	}
	if !slices.Equal(res.Kept, []string{"README.md"}) {
		t.Fatalf("the kept file must be reported: %+v", res)
	}
}

func TestAFileDeletedOnTheRemoteIsRemovedHere(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 3)
	a, b, admin := m[0], m[1], m[2]
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a"}`)
	mustSync(t, options(a, "host-a"))
	mustSync(t, options(b, "host-b"))
	if _, err := os.Stat(filepath.Join(b, "host-a.jsonl")); err != nil {
		t.Fatal("machine b did not receive machine a's file")
	}
	// Machine a is decommissioned and its file removed from the journal.
	commitByHand(t, admin, func(dir string) { run(t, dir, "rm", "--quiet", "host-a.jsonl") })

	res := mustSync(t, options(b, "host-b"))
	if _, err := os.Stat(filepath.Join(b, "host-a.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file deleted on the remote is still here (%v): %+v", err, res)
	}
	if status := run(t, b, "status", "--porcelain"); status != "" {
		t.Fatalf("the clone should match the remote, git status says:\n%s", status)
	}
}

func TestACRLFCheckoutOfThisHostsFileStillPublishes(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(m[0], "host-a"))

	// The same machine re-clones with core.autocrlf=true, the git for Windows
	// default, which checks this host's file out with CRLF line endings.
	clone := filepath.Join(t.TempDir(), "reclone")
	run(t, t.TempDir(), "clone", "--quiet", "-c", "core.autocrlf=true", remote, clone)
	identify(t, clone)
	path := filepath.Join(clone, "host-a.jsonl")
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "\r\n") {
		t.Fatalf("the test needs a CRLF checkout, got %q", got)
	}
	appendLines(t, path, `{"id":"hive:2"}`)

	if res := mustSync(t, options(clone, "host-a")); res.Published != 1 {
		t.Fatalf("%+v", res)
	}
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:1\"}\n{\"id\":\"hive:2\"}\n" {
		t.Fatalf("remote has %q, want both records with LF endings", got)
	}
}

func TestTheUsersOwnSSHCommandIsRespected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ssh is a shell script")
	}
	// Each case below chooses its own ssh command, or none. A go command that
	// switched to go.mod's toolchain hands the test binary a GIT_SSH_COMMAND of
	// its own: cmd/go's modload.Init sets one whenever the user has none.
	for _, key := range []string{"GIT_SSH", "GIT_SSH_COMMAND"} {
		unsetenv(t, key)
	}
	cases := map[string]func(t *testing.T, clone, ssh string){
		"GIT_SSH":         func(t *testing.T, _, ssh string) { t.Setenv("GIT_SSH", ssh) },
		"GIT_SSH_COMMAND": func(t *testing.T, _, ssh string) { t.Setenv("GIT_SSH_COMMAND", ssh) },
		"core.sshCommand": func(t *testing.T, clone, ssh string) { run(t, clone, "config", "core.sshCommand", ssh) },
	}
	for name, choose := range cases {
		t.Run(name, func(t *testing.T) {
			_, m := fleet(t, 1)
			a := m[0]
			run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
			marker := filepath.Join(t.TempDir(), "invoked")
			ssh := filepath.Join(t.TempDir(), "my-ssh")
			write(t, ssh, "#!/bin/sh\ntouch '"+marker+"'\nexit 1\n")
			if err := os.Chmod(ssh, 0o755); err != nil {
				t.Fatal(err)
			}
			choose(t, a, ssh)
			if _, err := Sync(context.Background(), options(a, "host-a")); err == nil {
				t.Fatal("the stand-in ssh fails, so the sync should too")
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("the user's ssh command was overridden")
			}
		})
	}
	t.Run("none chosen", func(t *testing.T) {
		_, m := fleet(t, 1)
		o := options(m[0], "host-a")
		var fetch []string
		o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
			if slices.Contains(args, "fetch") {
				fetch = args
			}
			return Git(ctx, dir, stdin, args...)
		}
		mustSync(t, o)
		if !slices.Contains(fetch, "core.sshCommand=ssh -o BatchMode=yes") {
			t.Fatalf("with no ssh command chosen, ssh must run in batch mode; fetch ran with %q", fetch)
		}
	})
}

// unsetenv removes key from the environment until the test ends.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	if v, ok := os.LookupEnv(key); ok {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Setenv(key, v) })
	}
}

func TestARewrittenRemoteNamesTheWayBackAndKeepsUnpublishedRecords(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 2)
	a, admin := m[0], m[1]
	path := filepath.Join(a, "host-a.jsonl")
	appendLines(t, path, `{"id":"hive:1"}`)
	mustSync(t, options(a, "host-a"))
	first := run(t, a, "rev-parse", "HEAD")
	appendLines(t, path, `{"id":"hive:2"}`)
	mustSync(t, options(a, "host-a"))
	// Someone force-pushes the remote back to before the second record.
	run(t, admin, "fetch", "--quiet")
	run(t, admin, "push", "--quiet", "--force", "origin", first+":refs/heads/main")
	appendLines(t, path, `{"id":"hive:3"}`)

	_, err := Sync(context.Background(), options(a, "host-a"))
	if !errors.Is(err, ErrLocalCommits) || !strings.Contains(err.Error(), "reset --soft '@{upstream}'") {
		t.Fatalf("expected ErrLocalCommits naming the way back, got %v", err)
	}
	run(t, a, "reset", "--soft", "@{upstream}")
	if res := mustSync(t, options(a, "host-a")); res.Published != 2 {
		t.Fatalf("both records the rewrite dropped or never saw should go out: %+v", res)
	}
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:1\"}\n{\"id\":\"hive:2\"}\n{\"id\":\"hive:3\"}\n" {
		t.Fatalf("remote has %q", got)
	}
}

func TestASyncInterruptedWhileBringingFilesInIsRepairedByTheNext(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(a, "host-a"))

	o := options(b, "host-b")
	o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		if slices.Contains(args, "checkout") {
			return "", errors.New("interrupted")
		}
		return Git(ctx, dir, stdin, args...)
	}
	if _, err := Sync(context.Background(), o); err == nil {
		t.Fatal("the injected interruption should surface")
	}
	mustSync(t, options(b, "host-b"))
	if got, _ := os.ReadFile(filepath.Join(b, "host-a.jsonl")); string(got) != "{\"id\":\"hive:1\"}\n" {
		t.Fatalf("the next sync did not bring in what the interrupted one missed: %q", got)
	}
}

func TestARemoteLinkCannotMakeASyncDeleteOutsideTheClone(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	write(t, victim, "not the journal's\n")
	_, m := fleet(t, 2)
	a, admin := m[0], m[1]
	commitByHand(t, admin, func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "d", "victim.txt"), "tracked\n")
	})
	mustSync(t, options(a, "host-a"))
	// The remote replaces the directory with a link to somewhere else and
	// deletes the file that was in it.
	commitByHand(t, admin, func(dir string) {
		run(t, dir, "rm", "-r", "--quiet", "d")
		if err := os.Symlink(outside, filepath.Join(dir, "d")); err != nil {
			t.Fatal(err)
		}
	})

	mustSync(t, options(a, "host-a"))
	if got, err := os.ReadFile(victim); err != nil || string(got) != "not the journal's\n" {
		t.Fatalf("a file outside the clone was changed or deleted: %q, %v", got, err)
	}
}

func TestALinkMadeInTheCloneCannotRedirectARemoval(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	write(t, victim, "not the journal's\n")
	_, m := fleet(t, 2)
	a, admin := m[0], m[1]
	commitByHand(t, admin, func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "d", "victim.txt"), "tracked\n")
	})
	mustSync(t, options(a, "host-a"))
	if err := os.RemoveAll(filepath.Join(a, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a, "d")); err != nil {
		t.Fatal(err)
	}
	commitByHand(t, admin, func(dir string) { run(t, dir, "rm", "--quiet", "d/victim.txt") })

	mustSync(t, options(a, "host-a"))
	if got, err := os.ReadFile(victim); err != nil || string(got) != "not the journal's\n" {
		t.Fatalf("a removal followed a link out of the clone: %q, %v", got, err)
	}
}

func TestARemovalAnInterruptedSyncMissedIsDoneByTheNext(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 3)
	a, b, admin := m[0], m[1], m[2]
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a"}`)
	mustSync(t, options(a, "host-a"))
	mustSync(t, options(b, "host-b"))
	commitByHand(t, admin, func(dir string) { run(t, dir, "rm", "--quiet", "host-a.jsonl") })

	o := options(b, "host-b")
	o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		// The branch has moved; nothing has been brought in yet.
		if slices.Contains(args, "diff-index") {
			return "", errors.New("interrupted")
		}
		return Git(ctx, dir, stdin, args...)
	}
	if _, err := Sync(context.Background(), o); err == nil {
		t.Fatal("the injected interruption should surface")
	}
	res := mustSync(t, options(b, "host-b"))
	if _, err := os.Stat(filepath.Join(b, "host-a.jsonl")); !errors.Is(err, os.ErrNotExist) || len(res.Kept) != 0 {
		t.Fatalf("the next sync should finish the removal without reporting it as kept (%v): %+v", err, res)
	}
	if status := run(t, b, "status", "--porcelain"); status != "" {
		t.Fatalf("git status says:\n%s", status)
	}
}

func TestAKeptFileSurvivesTheRemoteSwappingADirectoryAndAFile(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		before func(t *testing.T, dir string) // what the remote starts with
		edit   string                         // the file edited on this machine
		after  func(t *testing.T, dir string) // how the remote reshapes it
	}{
		"directory becomes a file": {
			before: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, "d", "k"), "remote\n")
			},
			edit: filepath.Join("d", "k"),
			after: func(t *testing.T, dir string) {
				run(t, dir, "rm", "-r", "--quiet", "d")
				write(t, filepath.Join(dir, "d"), "now a file\n")
			},
		},
		"file becomes a directory": {
			before: func(t *testing.T, dir string) { write(t, filepath.Join(dir, "f"), "remote\n") },
			edit:   "f",
			after: func(t *testing.T, dir string) {
				run(t, dir, "rm", "--quiet", "f")
				if err := os.Mkdir(filepath.Join(dir, "f"), 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, "f", "y"), "now inside a directory\n")
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, m := fleet(t, 2)
			a, admin := m[0], m[1]
			commitByHand(t, admin, func(dir string) { c.before(t, dir) })
			mustSync(t, options(a, "host-a"))
			write(t, filepath.Join(a, c.edit), "edited on this machine\n")
			commitByHand(t, admin, func(dir string) { c.after(t, dir) })

			res := mustSync(t, options(a, "host-a"))
			got, err := os.ReadFile(filepath.Join(a, c.edit))
			if err != nil || string(got) != "edited on this machine\n" {
				t.Fatalf("a file reported as kept was lost (%v, %q): %+v", err, got, res)
			}
			if !slices.Contains(res.Kept, filepath.ToSlash(c.edit)) {
				t.Fatalf("the edited file must be reported as kept: %+v", res)
			}
		})
	}
}

// git must not start anything meant to outlive the command, such as an
// fsmonitor daemon, since on Windows everything git leaves running is ended
// with it. A hook stands in for the daemon: it records that git consulted it.
func TestASyncNeverConsultsAnFsmonitor(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 1)
	a := m[0]
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor-hook")
	write(t, hook, "#!/bin/sh\n: > '"+filepath.ToSlash(marker)+"'\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, a, "config", "core.fsmonitor", filepath.ToSlash(hook))
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(a, "host-a"))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git consulted core.fsmonitor during a sync")
	}
}

// Before git 2.36, core.fsmonitor was read only as a hook's path, so "false"
// would name a hook to run; an empty value means no hook on every git version
// (config.c at v2.35.0, fsmonitor-settings.c at v2.43.0). The test above shows
// the empty value turns fsmonitor off on the git installed here.
func TestFsmonitorIsTurnedOffInAWayOldGitUnderstands(t *testing.T) {
	t.Parallel()
	for i := 0; i+1 < len(gitConfig); i += 2 {
		if gitConfig[i] == "-c" && strings.HasPrefix(gitConfig[i+1], "core.fsmonitor=") {
			if v := strings.TrimPrefix(gitConfig[i+1], "core.fsmonitor="); v != "" {
				t.Fatalf("core.fsmonitor=%q: git before 2.36 runs that as a hook", v)
			}
			return
		}
	}
	t.Fatalf("git is run without core.fsmonitor turned off: %q", gitConfig)
}

// A git hook, or anything run from one, has GIT_DIR and friends in its
// environment, pointing at the repository the hook belongs to. fleetd run there
// must still sync its own journal clone and nothing else.
func TestTheCallersGitEnvironmentCannotRedirectASync(t *testing.T) {
	for _, withWorkTree := range []bool{false, true} {
		t.Run(fmt.Sprintf("work tree set too: %v", withWorkTree), func(t *testing.T) {
			journalRemote, m := fleet(t, 1)
			a := m[0]
			projectRemote, p := fleet(t, 1)
			t.Setenv("GIT_DIR", filepath.Join(p[0], ".git"))
			if withWorkTree {
				t.Setenv("GIT_WORK_TREE", p[0])
				t.Setenv("GIT_INDEX_FILE", filepath.Join(p[0], ".git", "index"))
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
			res, err := Sync(context.Background(), options(a, "host-a"))
			if got := remoteFile(t, projectRemote, "host-a.jsonl"); got != "" {
				t.Fatalf("the journal was pushed into the repository GIT_DIR names (sync returned %+v, %v)", res, err)
			}
			if err != nil || res.Published != 1 || remoteFile(t, journalRemote, "host-a.jsonl") == "" {
				t.Fatalf("the record did not reach the journal's own remote: %+v, %v", res, err)
			}
		})
	}
}

// Every variable git itself counts as local to a repository is cleared, so a
// newer git that adds one fails here rather than in the field.
func TestEveryRepositoryVariableGitKnowsIsCleared(t *testing.T) {
	t.Parallel()
	requireGit(t)
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(string(out)) {
		if name == "GIT_CONFIG_COUNT" {
			// Kept on purpose, as git itself keeps it for a command it runs in
			// another repository: see TestConfigurationTheEnvironmentSetsOnPurposeStillApplies.
			continue
		}
		if !slices.Contains(repositoryVariables, name) {
			t.Errorf("git reports %s as local to a repository, and fleetd would pass it on", name)
		}
	}
}

// Configuration the environment sets on purpose, through GIT_CONFIG_COUNT and
// its GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n, still applies: IT policy, CI or a
// sandbox may rewrite a URL, trust a directory or name an ssh command that way,
// and git never sets GIT_CONFIG_COUNT itself (a parent git's -c travels in
// GIT_CONFIG_PARAMETERS, which is dropped).
func TestConfigurationTheEnvironmentSetsOnPurposeStillApplies(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	run(t, a, "remote", "set-url", "origin", "journal-by-policy:journal")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+remote+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "journal-by-policy:journal")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	if res := mustSync(t, options(a, "host-a")); res.Published != 1 {
		t.Fatalf("result = %+v", res)
	}
	if remoteFile(t, remote, "host-a.jsonl") == "" {
		t.Fatal("the record was not published through the URL the environment rewrote")
	}
}

// git exports the dates of the commit it is making to its hooks, and an amend
// carries the original commit's. A sync run from such a hook dates its journal
// commit when the sync ran, not with the hook's dates.
func TestJournalCommitsAreDatedWhenTheSyncRan(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	t.Setenv("GIT_AUTHOR_DATE", "@1009843200 +0000")
	t.Setenv("GIT_COMMITTER_DATE", "@1009843200 +0000")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	before := time.Now().Add(-time.Minute).Unix()
	mustSync(t, options(a, "host-a"))
	out, err := exec.Command("git", "--git-dir", remote, "log", "-1", "--format=%at %ct", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(out)) {
		if at, _ := strconv.ParseInt(field, 10, 64); at < before {
			t.Fatalf("the journal commit is dated %s (Unix seconds), not when the sync ran", out)
		}
	}
}

// A clone that nothing packs grows without end: every sync adds loose objects,
// and git's automatic gc is off for every command a sync runs, so that none
// starts inside a fetch. Once the clone holds packLimit loose objects, a
// successful sync packs it.
func TestAJournalCloneIsPackedOnceItHoldsPackLimitLooseObjects(t *testing.T) {
	saved := packLimit
	t.Cleanup(func() { packLimit = saved })
	packLimit = 12
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	for i := range 6 {
		appendLines(t, filepath.Join(b, "host-b.jsonl"), fmt.Sprintf(`{"id":"hive:b%d"}`, i))
		mustSync(t, options(b, "host-b"))
		appendLines(t, filepath.Join(a, "host-a.jsonl"), fmt.Sprintf(`{"id":"hive:a%d"}`, i))
		mustSync(t, options(a, "host-a"))
	}
	out := run(t, a, "count-objects", "-v")
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "count: "); ok {
			if loose, _ := strconv.Atoi(strings.TrimSpace(v)); loose >= packLimit {
				t.Fatalf("machine a's clone holds %d loose objects after 12 syncs; it should have been packed at %d", loose, packLimit)
			}
			return
		}
	}
	t.Fatalf("count-objects said %q", out)
}

// A gc the deadline kills partway leaves behind every lock it held, and a lock on
// a ref, a reflog or the commit graph fails every later sync until it is ten
// minutes old. So the gc a sync runs packs objects and nothing else.
func TestPackingTouchesOnlyObjects(t *testing.T) {
	saved := packLimit
	t.Cleanup(func() { packLimit = saved })
	packLimit = 12
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	packed := false
	for i := 0; i < 20 && !packed; i++ {
		appendLines(t, filepath.Join(b, "host-b.jsonl"), fmt.Sprintf(`{"id":"hive:b%d"}`, i))
		mustSync(t, options(b, "host-b"))
		appendLines(t, filepath.Join(a, "host-a.jsonl"), fmt.Sprintf(`{"id":"hive:a%d"}`, i))
		packed = mustSync(t, options(a, "host-a")).Packed
	}
	if !packed {
		t.Fatal("machine a's clone was never packed")
	}
	gitDir := filepath.Join(a, ".git")
	branch := run(t, a, "symbolic-ref", "--short", "HEAD")
	if _, err := os.Stat(filepath.Join(gitDir, "refs", "heads", branch)); err != nil {
		t.Errorf("gc packed the branch's ref, taking its lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "commit-graph")); err == nil {
		t.Error("gc wrote a commit graph, taking its lock")
	}
}

// Journal commits are fleetd's, not the person's: a PC's git identity may be a
// private address GitHub refuses to publish (GH007), may be missing altogether,
// and does not belong in a shared journal's history either way.
func TestJournalCommitsCarryFleetdsIdentityNotThePCs(t *testing.T) {
	remote, m := fleet(t, 1)
	a := m[0]
	run(t, a, "config", "--unset", "user.name")
	run(t, a, "config", "--unset", "user.email")
	t.Setenv("GIT_AUTHOR_NAME", "Someone Private")
	t.Setenv("GIT_AUTHOR_EMAIL", "someone@example.com")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(a, "host-a"))
	out, err := exec.Command("git", "--git-dir", remote, "log", "-1", "--format=%an <%ae>|%cn <%ce>", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(out)), "fleetd <fleetd@fleetd.invalid>|fleetd <fleetd@fleetd.invalid>"; got != want {
		t.Fatalf("journal commit identity = %q, want %q", got, want)
	}
}

// git's automatic gc and maintenance can take longer than a sync's deadline, and
// one killed partway leaves lock files that fail every later sync. A sync never
// starts them; git's own trace shows whether it did.
func TestASyncNeverStartsGitsAutomaticMaintenance(t *testing.T) {
	remote, m := fleet(t, 2)
	a, b := m[0], m[1]
	// The stand-in remote is a local repository, so its own receive-pack would
	// start its own gc after a push, which a real remote does on its server.
	run(t, a, "--git-dir", remote, "config", "receive.autogc", "false")
	run(t, a, "config", "gc.auto", "1")
	run(t, a, "config", "maintenance.auto", "true")
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b"}`)
	mustSync(t, options(b, "host-b"))
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a"}`)
	trace := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	if res := mustSync(t, options(a, "host-a")); res.Received != 1 {
		t.Fatalf("expected to receive b's record: %+v", res)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("git wrote no trace, so this test proves nothing: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `"event":"child_start"`) && strings.Contains(line, `"--auto"`) &&
			(strings.Contains(line, `"maintenance"`) || strings.Contains(line, `"gc"`)) {
			t.Fatalf("a sync started git's automatic maintenance: %s", line)
		}
	}
}

// A git command the deadline kills can leave its lock file, which git never
// removes: every later sync of that clone then fails, silently, inside a hook.
// fleetd owns the clone and holds its own sync lock, so a git lock left for
// longer than staleLock is a leftover, and is removed. A newer one is left
// alone, since a git command may still be using it.
func TestALockAKilledGitLeftBehindIsClearedOnceStale(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b"}`)
	mustSync(t, options(b, "host-b"))
	for _, name := range []string{"index.lock", filepath.Join("refs", "heads", "main.lock")} {
		write(t, filepath.Join(a, ".git", name), "")
	}

	// Fresh locks may belong to a git command that is still running.
	if _, err := Sync(context.Background(), options(a, "host-a")); err == nil {
		t.Fatal("a sync ran over a fresh git lock")
	}
	for _, name := range []string{"index.lock", filepath.Join("refs", "heads", "main.lock")} {
		if _, err := os.Stat(filepath.Join(a, ".git", name)); err != nil {
			t.Fatalf("a fresh lock was removed: %v", err)
		}
		old := time.Now().Add(-staleLock - time.Minute)
		if err := os.Chtimes(filepath.Join(a, ".git", name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	res := mustSync(t, options(a, "host-a"))
	if res.Received != 1 {
		t.Fatalf("expected b's record once the stale locks were cleared: %+v", res)
	}
	want := []string{"index.lock", filepath.Join("refs", "heads", "main.lock")}
	if !slices.Equal(res.Cleared, want) {
		t.Fatalf("cleared %v, want %v", res.Cleared, want)
	}
	if _, err := os.Stat(filepath.Join(a, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("the stale index.lock is still there: %v", err)
	}
}

// A push that lost a race to another machine is retried however the remote
// words it. GitHub reports a race it catches while updating the ref as
// "[remote rejected] ... (cannot lock ref ...)", not "[rejected]". A rejection
// for any other reason, a declined hook or a protected branch, is not a race,
// and retrying it would only repeat it.
func TestOnlyARaceIsRetried(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"To github.com:o/journal.git\n!\trefs/heads/main:refs/heads/main\t[rejected] (fetch first)\nDone\n":                                                            true,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (cannot lock ref 'refs/heads/main': is at 3f1c but expected 1a2b)\n":                                    true,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (failed to update ref)\n":                                                                               true,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (incorrect old value provided)\n":                                                                       true,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (pre-receive hook declined)\n":                                                                          false,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (protected branch hook declined)\n":                                                                     false,
		"remote: Permission to o/journal.git denied to someone.\nfatal: unable to access 'https://github.com/o/journal.git/': The requested URL returned error: 403\n": false,
	} {
		if got := lostRace(out); got != want {
			t.Errorf("lostRace(%q) = %v, want %v", out, got, want)
		}
	}
}

// Only a push the remote declines, by a hook, branch protection or a ruleset,
// is refused for good. A race and a failure on the remote's side, such as its
// storage, are not: a later push can get past them.
func TestOnlyADeclinedPushIsRefused(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (pre-receive hook declined)\n":                                                                          true,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (protected branch hook declined)\n":                                                                     true,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (push declined due to repository rule violations)\n":                                                    true,
		"To https://example.invalid/declined/journal.git\n!\trefs/heads/main:refs/heads/main\t[remote rejected] (pre-receive hook declined)\nDone\n":                   true,
		"To https://example.invalid/declined/journal.git\n!\trefs/heads/main:refs/heads/main\t[remote rejected] (failed to update ref)\nDone\n":                        false,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (unpacker error)\n":                                                                                     false,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (cannot lock ref 'refs/heads/main': is at 3f1c but expected 1a2b)\n":                                    false,
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (failed to update ref)\n":                                                                               false,
		"To github.com:o/journal.git\n!\trefs/heads/main:refs/heads/main\t[rejected] (fetch first)\nDone\n":                                                            false,
		"remote: Permission to o/journal.git denied to someone.\nfatal: unable to access 'https://github.com/o/journal.git/': The requested URL returned error: 403\n": false,
	} {
		if got := refused(out); got != want {
			t.Errorf("refused(%q) = %v, want %v", out, got, want)
		}
	}
}

// git for Windows checks files out with CRLF line endings. A file that holds only
// the start of git's copy in that form has nothing of its own either, and a sync
// brings it up to date.
func TestASyncUpdatesACRLFCopyThatHoldsOnlyTheStartOfGitsCopy(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	run(t, a, "config", "core.autocrlf", "true")
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:1"}`, `{"id":"hive:2"}`)
	mustSync(t, options(b, "host-b"))
	mustSync(t, options(a, "host-a"))
	write(t, filepath.Join(a, "host-b.jsonl"), "{\"id\":\"hive:1\"}\r\n")
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:3"}`)
	mustSync(t, options(b, "host-b"))
	if res := mustSync(t, options(a, "host-a")); len(res.Kept) != 0 {
		t.Fatalf("a CRLF copy behind git's was kept: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(a, "host-b.jsonl"))
	want := "{\"id\":\"hive:1\"}\n{\"id\":\"hive:2\"}\n{\"id\":\"hive:3\"}\n"
	if err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Fatalf("host-b.jsonl holds %q (%v), want %q", got, err, want)
	}
}

// A CRLF copy of short lines that holds only the start of git's copy can be
// larger than git's LF copy, by up to twice: it is still read, and updated.
func TestACRLFCopyLargerThanGitsCopyCanStillHoldOnlyItsStart(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	run(t, a, "config", "core.autocrlf", "true")
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf(`{"id":"hive:%d"}`, i))
	}
	appendLines(t, filepath.Join(b, "host-b.jsonl"), lines...)
	mustSync(t, options(b, "host-b"))
	mustSync(t, options(a, "host-a"))
	// The first 29 lines, with CRLF endings: larger than git's 30 LF lines.
	short := strings.Join(lines[:29], "\r\n") + "\r\n"
	if git := len(strings.Join(lines, "\n")) + 1; len(short) <= git {
		t.Fatalf("the CRLF copy is %d bytes, git's %d; the test needs it larger", len(short), git)
	}
	write(t, filepath.Join(a, "host-b.jsonl"), short)
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:31"}`)
	mustSync(t, options(b, "host-b"))
	if res := mustSync(t, options(a, "host-a")); len(res.Kept) != 0 {
		t.Fatalf("a CRLF copy behind git's was kept: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(a, "host-b.jsonl"))
	want := strings.Join(append(lines, `{"id":"hive:31"}`), "\n") + "\n"
	if err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Fatalf("host-b.jsonl holds %q (%v), want %q", got, err, want)
	}
}

// Another push that lands on the remote between this push's check and its update
// is reported by the remote as [remote rejected], not by git as [rejected]. It is
// a race like any other: the sync tries again and publishes.
func TestARaceTheRemoteReportsIsRetried(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	write(t, hook, `#!/bin/sh
if [ ! -f "$GIT_DIR/raced" ]; then
	touch "$GIT_DIR/raced"
	unset GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
	tip=$(git rev-parse refs/heads/main)
	c=$(GIT_AUTHOR_NAME=o GIT_AUTHOR_EMAIL=o@example.invalid GIT_COMMITTER_NAME=o GIT_COMMITTER_EMAIL=o@example.invalid git commit-tree "$tip^{tree}" -p "$tip" -m "another machine")
	git update-ref refs/heads/main "$c" "$tip"
fi
`)
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	res, err := Sync(context.Background(), options(m[0], "host-a"))
	if err != nil || res.Attempts != 2 || res.Published != 1 {
		t.Fatalf("result %+v, err %v; want the record published on the second attempt", res, err)
	}
}

// A file that holds only the start of git's own copy, such as one restored from
// an older backup, has no change of its own. A sync brings it up to date, both
// when the remote changed it since and when it did not.
func TestASyncUpdatesAFileThatHoldsOnlyTheStartOfGitsCopy(t *testing.T) {
	t.Parallel()
	_, m := fleet(t, 3)
	a, b, c := m[0], m[1], m[2]
	for _, host := range []struct{ dir, name string }{{b, "host-b"}, {c, "host-c"}} {
		appendLines(t, filepath.Join(host.dir, host.name+".jsonl"), `{"id":"hive:1"}`, `{"id":"hive:2"}`)
		mustSync(t, options(host.dir, host.name))
	}
	mustSync(t, options(a, "host-a"))
	for _, name := range []string{"host-b.jsonl", "host-c.jsonl"} {
		write(t, filepath.Join(a, name), "{\"id\":\"hive:1\"}\n")
	}
	appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:3"}`)
	mustSync(t, options(b, "host-b"))

	res := mustSync(t, options(a, "host-a"))
	if len(res.Kept) != 0 {
		t.Fatalf("a file behind git's copy was kept: %+v", res)
	}
	for name, want := range map[string]string{
		"host-b.jsonl": "{\"id\":\"hive:1\"}\n{\"id\":\"hive:2\"}\n{\"id\":\"hive:3\"}\n",
		"host-c.jsonl": "{\"id\":\"hive:1\"}\n{\"id\":\"hive:2\"}\n",
	} {
		if got, _ := os.ReadFile(filepath.Join(a, name)); string(got) != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	if status := run(t, a, "status", "--porcelain"); status != "" {
		t.Fatalf("git status after the sync:\n%s", status)
	}
}

// A sync whose second look for a gone branch cannot fetch says why, rather than
// that the branch is gone: the network may be down while the branch is back.
func TestASecondLookThatCannotFetchSaysWhy(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	run(t, a, "--git-dir", remote, "branch", "-m", "main", "trunk")
	run(t, a, "fetch", "--quiet", "--prune")
	down := errors.New("network down")
	o := options(a, "host-a")
	o.Run = func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		if slices.Contains(args, "fetch") {
			return "", down
		}
		return Git(ctx, dir, stdin, args...)
	}
	if _, err := Sync(context.Background(), o); !errors.Is(err, down) || errors.Is(err, ErrNoUpstream) {
		t.Fatalf("sync: %v, want the fetch's own error", err)
	}
}
