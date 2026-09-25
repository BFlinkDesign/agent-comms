//go:build windows

package gitsync

import (
	"context"
	"errors"
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
	_, m := fleet(t, 1)
	a := m[0]
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	t.Setenv("GIT_SSH_COMMAND", "sleep 30; true")
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Sync(ctx, options(a, "host-a"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second+waitDelay+2*time.Second {
		t.Fatalf("a hung remote held the sync for %v", elapsed)
	}
}
