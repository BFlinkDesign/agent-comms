//go:build !windows

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// returnsWithin runs f and fails the test if it has not returned within d: a
// hang, not a wrong answer, is what these tests look for.
func returnsWithin(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s had not returned after %s", what, d)
	}
}

// An account able to write beside the journal directory could plant a FIFO
// where the salts note goes. Opening one waits for a writer that never comes;
// a sync must not wait with it, past its --timeout and holding its lock.
func TestAFIFOAtTheSaltsNoteDoesNotHoldUpASync(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(saltsNote(dir), 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	fleetFileAged(t, dir)
	var err error
	returnsWithin(t, 30*time.Second, "a sync with a FIFO where the salts note goes", func() {
		_, _, err = exec(t, "sync", "--dir", dir, "--timeout", "10s")
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// The same for fleetd-hook.log: `where` reads it, and a hook with a problem to
// report appends to it.
func TestAFIFOAtTheHookLogDoesNotHoldUpWhereOrAHook(t *testing.T) {
	hookEnv(t)
	remote := emptyJournalRemote(t)
	root := t.TempDir()
	t.Setenv("COMMS_CHANNELS", root)
	dir := filepath.Join(root, "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(logBeside(dir), 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	returnsWithin(t, 30*time.Second, "fleetd where with a FIFO where the hook log goes", func() {
		_, _, _ = exec(t, "where", "--dir", dir)
	})
	feedStdin(t, "not json")
	var err error
	returnsWithin(t, 30*time.Second, "a hook with a problem to log and a FIFO where the log goes", func() {
		_, _, err = exec(t, "hook", "claude")
	})
	if err != nil {
		t.Fatalf("the hook returned %v; a hook must exit 0 whatever happens", err)
	}
}
