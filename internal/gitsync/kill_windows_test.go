//go:build windows

package gitsync

import (
	"context"
	"errors"
	"os"
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
	// sh accepts a forward-slashed Windows path. git's check of whether this
	// is OpenSSH passes -G, which the test binary refuses at once; the real
	// connection then starts the sleeper.
	t.Setenv("GIT_SSH_COMMAND", "'"+filepath.ToSlash(exe)+"' -test.run='^TestSleeperHelper$'")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err = Sync(ctx, options(a, "host-a"))
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
	if elapsed > time.Second+waitDelay+2*time.Second {
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
