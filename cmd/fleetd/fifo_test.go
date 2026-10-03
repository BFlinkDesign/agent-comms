//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
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
	t.Parallel()
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

// fleetd.json comes from the remote, and a symbolic link there is checked out
// as one: a link to a FIFO must not hold up a record, which reads fleetd.json
// for its salt.
func TestAFleetFileLinkedToAFIFODoesNotHoldUpARecord(t *testing.T) {
	t.Parallel()
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	editor := filepath.Join(t.TempDir(), "editor")
	gitIn(t, filepath.Dir(editor), "clone", "--quiet", remote, editor)
	gitIn(t, editor, "rm", "--quiet", gitsync.FleetFile)
	if err := os.Symlink(fifo, filepath.Join(editor, gitsync.FleetFile)); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	gitIn(t, editor, "add", gitsync.FleetFile)
	gitIn(t, editor, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "a link")
	gitIn(t, editor, "push", "--quiet")
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	var err error
	returnsWithin(t, 30*time.Second, "a record with fleetd.json linked to a FIFO", func() {
		_, _, err = exec(t, "record", "--dir", dir, "--type", "note", "--note", "while fleetd.json is a link")
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
}

// init on a clone whose fleetd.json is a link to a FIFO replaces it, rather than
// reading through it for a salt to note and waiting for a writer.
func TestInitOnACloneWhoseFleetFileLinksToAFIFOReturns(t *testing.T) {
	t.Parallel()
	remote := emptyJournalRemote(t)
	if _, _, err := exec(t, "init", "--dir", filepath.Join(t.TempDir(), "first"), "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	path := filepath.Join(dir, gitsync.FleetFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	var err error
	returnsWithin(t, 30*time.Second, "init with fleetd.json linked to a FIFO", func() {
		_, _, err = exec(t, "init", "--dir", dir, remote)
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("after init fleetd.json is %v (%v), want a regular file", info.Mode(), err)
	}
}
