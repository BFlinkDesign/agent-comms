//go:build windows

package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	saved := assignJob
	t.Cleanup(func() { assignJob = saved })
	assignJob = func(syscall.Handle, int) error { return errors.New("cannot join for this test") }
	fallbackDeadline(t)
}

// fallbackDeadline syncs against a remote whose ssh hangs past the deadline and
// checks that the sync returns near it. Outside a job, what git started is not
// killed: the real git.exe behind Git for Windows' launcher keeps its working
// directory in the clone, inside the test's TempDir, which Windows will not
// delete while a process uses it. So the stand-in ssh outlives the deadline
// bound, which only WaitDelay can then meet, but ends soon after, and the test
// waits for it before its TempDir is removed.
func fallbackDeadline(t *testing.T) {
	t.Helper()
	const hang = 8 * time.Second // longer than the bound checked below
	_, m := fleet(t, 1)
	a := m[0]
	done := filepath.Join(t.TempDir(), "ssh-done")
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	t.Setenv("GIT_SSH_COMMAND", fmt.Sprintf("sleep %d; : > '%s'; false", int(hang/time.Second), filepath.ToSlash(done)))
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Sync(ctx, options(a, "host-a"))
	elapsed := time.Since(start)

	// Let the orphans finish whatever the result, so TempDir can be removed.
	for wait := time.Now().Add(hang + 10*time.Second); time.Now().Before(wait); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(done); err == nil {
			break
		}
	}
	time.Sleep(time.Second) // for git to exit after its ssh has

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
	if bound := time.Second + waitDelay + 2*time.Second; bound >= hang || elapsed > bound {
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
// cut off at the deadline.
func TestARetriedGitIsStillCutOffNearTheDeadline(t *testing.T) {
	saved := resumeGit
	t.Cleanup(func() { resumeGit = saved })
	resumeGit = func(int) error { return errors.New("cannot resume for this test") }
	fallbackDeadline(t)
}
