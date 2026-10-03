//go:build !windows

package gitsync

import (
	"path/filepath"
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
