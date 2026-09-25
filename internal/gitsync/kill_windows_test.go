//go:build windows

package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// syncOnce publishes one record from a fresh clone and returns how many git
// commands ran outside a job while it did.
func syncOnce(t *testing.T) int64 {
	t.Helper()
	remote, m := fleet(t, 1)
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	before := jobFallbacks.Load()
	if res := mustSync(t, options(m[0], "host-a")); res.Published != 1 || remoteFile(t, remote, "host-a.jsonl") == "" {
		t.Fatalf("the record was not published: %+v", res)
	}
	return jobFallbacks.Load() - before
}

func TestEveryGitCommandRunsInAJob(t *testing.T) {
	if n := syncOnce(t); n != 0 {
		t.Fatalf("%d git commands ran outside a job; the whole-tree kill did not apply to them", n)
	}
}

func TestAJobThatCannotBeCreatedFallsBackAndTheSyncStillWorks(t *testing.T) {
	saved := createJob
	t.Cleanup(func() { createJob = saved })
	createJob = func() (syscall.Handle, error) { return 0, errors.New("no job for this test") }
	if n := syncOnce(t); n == 0 {
		t.Fatal("git ran without a job and nothing counted the fallback")
	}
}

func TestAProcessThatCannotJoinTheJobFallsBackAndTheSyncStillWorks(t *testing.T) {
	saved := assignJob
	t.Cleanup(func() { assignJob = saved })
	assignJob = func(syscall.Handle, int) error { return errors.New("cannot join for this test") }
	if n := syncOnce(t); n == 0 {
		t.Fatal("git ran outside its job and nothing counted the fallback")
	}
}

// Outside a job, cancelling still kills git itself, as before job objects were
// used, and WaitDelay still returns control.
func TestAFallbackSyncIsStillCutOffNearTheDeadline(t *testing.T) {
	releaseTempDirs(t)
	outsideFleetdsJob(t)
	fallbackDeadline(t)
}

// fallbackDeadline syncs against a remote whose ssh is the sleeper, which
// outlasts any deadline, and checks that the sync returns near the deadline.
// Outside a job, cancelling kills only Git for Windows' launcher: the real git
// behind it, its sh and the sleeper keep running, which only WaitDelay can cut
// the sync loose from. They keep their working directory in the clone, so the
// sleeper is killed when the test ends, which ends the rest, before the TempDirs
// are removed.
func fallbackDeadline(t *testing.T) {
	t.Helper()
	_, m := fleet(t, 1)
	a := m[0]
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "sleeper.pid")
	t.Setenv(sleeperEnv, pidfile)
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidfile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				killSleeper(t, pid)
			}
		}
	})
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	// sh accepts a forward-slashed Windows path. The simple variant makes git
	// start the stand-in once, for the real connection, holding git's output.
	t.Setenv("GIT_SSH_VARIANT", "simple")
	t.Setenv("GIT_SSH_COMMAND", "'"+filepath.ToSlash(exe)+"' -test.run='^TestSleeperHelper$'")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	const deadline = 3 * time.Second // long enough to reach ssh even when every git command runs twice
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	_, err = Sync(ctx, options(a, "host-a"))
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
	if _, serr := os.Stat(pidfile); serr != nil {
		t.Fatalf("the sync never reached the hung remote, so this proves nothing: %v", serr)
	}
	if elapsed > deadline+waitDelay+2*time.Second {
		t.Fatalf("a hung remote held the sync for %v", elapsed)
	}
}

func TestAGitThatCannotBeResumedIsRunAgainAndTheSyncStillWorks(t *testing.T) {
	saved := resumeGit
	t.Cleanup(func() { resumeGit = saved })
	resumeGit = func(int) error { return errors.New("cannot resume for this test") }
	before := resumeFallbacks.Load()
	if n := syncOnce(t); n == 0 {
		t.Fatal("git ran outside its job and nothing counted the fallback")
	}
	if resumeFallbacks.Load() == before {
		t.Fatal("git could not be resumed and nothing counted it")
	}
}

// The first resume can fail after git joined the job; the retry must still be
// cut off at the deadline. The retried git runs outside any job, so what it
// leaves running ends only when the sleeper is killed.
func TestARetriedGitIsStillCutOffNearTheDeadline(t *testing.T) {
	releaseTempDirs(t)
	saved := resumeGit
	t.Cleanup(func() { resumeGit = saved })
	resumeGit = func(int) error { return errors.New("cannot resume for this test") }
	fallbackDeadline(t)
}

// If git can be neither resumed nor killed (endpoint software that filters both,
// say), waiting for it would last forever; the command must fail instead.
func TestAGitThatCanBeNeitherResumedNorKilledFailsInsteadOfHanging(t *testing.T) {
	savedResume, savedKill := resumeGit, killGit
	t.Cleanup(func() { resumeGit, killGit = savedResume, savedKill })
	resumeGit = func(int) error { return errors.New("cannot resume for this test") }
	killGit = func(*os.Process) error { return errors.New("cannot kill for this test") }
	done := make(chan error, 1)
	go func() {
		_, err := Git(context.Background(), t.TempDir(), nil, "version")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("git ran although it could be neither resumed nor killed")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("git could be neither resumed nor killed, and the command waited for it")
	}
}

// The negative control for TestCancellingASyncKillsEveryProcessGitStarted: with
// git kept out of its job, the same cancelled sync leaves the sleeper running.
// So the job, not some side effect of cancelling, is what ends git's tree, and
// processGone reports a live process, by the id the sleeper wrote, as live.
func TestWithoutTheJobTheSleeperOutlivesTheCancelledSync(t *testing.T) {
	releaseTempDirs(t)
	// The test's own job ends the sleeper's tree when the test does; it does
	// nothing when the sync is cancelled, so it cannot kill the sleeper early.
	outsideFleetdsJob(t)
	s, err := syncUntilSleeperRuns(t, "sleep 3; true")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation, got %v", err)
	}
	time.Sleep(time.Second) // as long as a kill could take to land
	if processGone(t, s.pid) {
		t.Fatalf("process %d ended although git ran outside any job: the tree-kill test cannot tell a job from no job", s.pid)
	}
}

// If the context ends after git is started, suspended, but before it joins its
// job, cancel can only kill git alone. When that kill is refused (by endpoint
// software, say), Wait would last as long as git stays suspended, which may be
// forever, so the command returns at once instead.
func TestACancelBeforeGitJoinsItsJobDoesNotWaitWhenGitCannotBeKilled(t *testing.T) {
	releaseTempDirs(t)
	requireGit(t)
	dir := t.TempDir()
	savedKill, savedAfter := killGit, afterStart
	var started *os.Process
	t.Cleanup(func() {
		killGit, afterStart = savedKill, savedAfter
		if started != nil {
			_ = started.Kill() // the real kill, so the suspended git does not outlive the test
			for deadline := time.Now().Add(10 * time.Second); !processGone(t, started.Pid) && time.Now().Before(deadline); {
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	killGit = func(*os.Process) error { return errors.New("cannot kill for this test") }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterStart = func(tr *tree) {
		started = tr.cmd.Process
		cancel()
		for deadline := time.Now().Add(10 * time.Second); !tr.isCanceled() && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	newCmd := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, "git", "version")
		cmd.Dir = dir
		cmd.WaitDelay = waitDelay
		return cmd
	}
	start := time.Now()
	err := runTree(newCmd)
	if !errors.Is(err, errUnkillable) {
		t.Fatalf("err = %v, want errUnkillable", err)
	}
	if elapsed := time.Since(start); elapsed >= waitDelay {
		t.Fatalf("waited %v for a git that could not be killed", elapsed)
	}
}

// Kill-on-close ends what git leaves running when a git command returns, not
// only when it is cancelled: here a daemon detached from git's output, as an ssh
// ControlPersist master or a credential helper would be.
func TestTheJobEndsWhatGitLeavesRunningWhenItReturns(t *testing.T) {
	releaseTempDirs(t)
	_, m := fleet(t, 1)
	a := m[0]
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "sleeper.pid")
	t.Setenv(sleeperEnv, pidfile)
	t.Setenv("GIT_SSH_VARIANT", "simple")
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	// The stand-in starts the sleeper detached from its output, waits until it
	// has written its id, and ends, so git returns while the sleeper runs on.
	t.Setenv("GIT_SSH_COMMAND", fmt.Sprintf(
		"'%s' -test.run='^TestSleeperHelper$' </dev/null >/dev/null 2>&1 & while [ ! -s '%s' ]; do sleep 0.1; done; true",
		filepath.ToSlash(exe), filepath.ToSlash(pidfile)))
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _ = Sync(ctx, options(a, "host-a")) // the stand-in speaks no git protocol, so the fetch fails
	if ctx.Err() != nil {
		t.Fatal("the sync ran into its deadline, so this does not show a normal return")
	}
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the sleeper never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killSleeper(t, pid) })
	for deadline := time.Now().Add(10 * time.Second); !processGone(t, pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d, left running by the git command, outlived it", pid)
		}
	}
}
