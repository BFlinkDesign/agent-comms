//go:build !windows

package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a salts file goes is neither read nor written, and opening it
// never waits for a writer or a reader that may never come.
func TestASaltsFileThatIsAFIFOIsNotWaitedOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "salts")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	type outcome struct {
		read []string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		read := ReadSalts(path)
		done <- outcome{read, AppendSalt(path, "x")}
	}()
	select {
	case o := <-done:
		if len(o.read) != 0 || o.err == nil {
			t.Fatalf("a FIFO as the salts file: read %q, noting gave %v; want nothing read and an error", o.read, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading or noting a salt where a FIFO stands had not returned after 10s")
	}
}

// A salts file far larger than memory, a sparse one say, is refused without
// being read.
func TestASparseSaltsFileFarLargerThanMemoryIsRefusedAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "salts")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 1<<40); err != nil {
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	type outcome struct {
		read []string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		read := ReadSalts(path)
		done <- outcome{read, AppendSalt(path, "x")}
	}()
	select {
	case o := <-done:
		if len(o.read) != 0 || o.err == nil {
			t.Fatalf("a 1 TiB salts file: read %q, noting gave %v; want nothing read and an error", o.read, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading or noting a salt in a 1 TiB salts file had not returned after 10s")
	}
}

// A file this machine changed that the remote changed too is kept as this
// machine's change, unless it holds only the start of git's copy. One more than
// twice the size of git's copy cannot, CRLF line endings and all, so it is kept
// without being read: a sparse file far larger than memory does not hold up a
// sync, or leave its lock behind.
func TestALocalFileFarLargerThanGitsCopyIsKeptUnread(t *testing.T) {
	_, m := fleet(t, 2)
	a, b := m[0], m[1]
	write(t, filepath.Join(b, "README.md"), "changed on b\n")
	run(t, b, "commit", "--quiet", "-am", "b changes the readme")
	run(t, b, "push", "--quiet")
	big := filepath.Join(a, "README.md")
	if err := os.Truncate(big, 1<<40); err != nil {
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Sync(context.Background(), options(a, "host-a"))
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil || !slices.Contains(o.res.Kept, "README.md") {
			t.Fatalf("sync with a 1 TiB README.md: %+v, %v; want it kept", o.res, o.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a sync with a 1 TiB README.md had not returned after 30s")
	}
	if info, err := os.Stat(big); err != nil || info.Size() != 1<<40 {
		t.Fatalf("the sync changed the kept README.md: %v", err)
	}
}
