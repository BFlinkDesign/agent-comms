//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
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

// The notes fleetd reads from the clone's git directory are read as every other
// file it keeps: the salt cache, which every sync reads under its lock where the
// journal directory lacks fleetd.json, the sync's outcome, which where and every
// hook read, and the sizes of the moved copies re-filed. A FIFO there is refused,
// never waited on.
func TestAFIFOAtANoteInTheGitDirectoryHoldsNothingUp(t *testing.T) {
	t.Parallel()
	for _, note := range []string{"the salt cache, with fleetd.json", "the salt cache, without fleetd.json",
		"the sync's outcome", "the re-filed sizes"} {
		t.Run(note, func(t *testing.T) {
			t.Parallel()
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			path := map[string]string{
				"the salt cache, with fleetd.json":    filepath.Join(dir, ".git", saltCacheName),
				"the salt cache, without fleetd.json": filepath.Join(dir, ".git", saltCacheName),
				"the sync's outcome":                  filepath.Join(dir, ".git", syncStatusFile),
				"the re-filed sizes":                  filepath.Join(dir, ".git", preInitDir, refiledName),
			}[note]
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("cannot make a FIFO here: %v", err)
			}
			if note == "the salt cache, without fleetd.json" {
				if err := os.Remove(filepath.Join(dir, gitsync.FleetFile)); err != nil {
					t.Fatal(err)
				}
			}
			// where first: a sync replaces the notes it writes.
			returnsWithin(t, 30*time.Second, "where with a FIFO at "+note, func() {
				_, _, _ = exec(t, "where", "--dir", dir)
			})
			returnsWithin(t, 30*time.Second, "a sync with a FIFO at "+note, func() {
				_, _, _ = exec(t, "sync", "--dir", dir, "--timeout", "10s")
			})
			if _, err := os.Lstat(filepath.Join(dir, ".git", "fleetd-sync.lock")); err == nil {
				t.Fatal("the sync left its lock behind")
			}
		})
	}
}

// A push can make any host file in the journal a symbolic link, which every
// clone's sync checks out as one: to a device that never ends, or to a FIFO that
// never answers. where reads every host file: it refuses the link, naming it,
// and still reports the other machines' records.
func TestAHostFileTheRemoteMadeALinkHoldsUpNothing(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"a device that never ends", "a FIFO"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "this machine's own record"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			to := "/dev/zero"
			if target == "a FIFO" {
				to = filepath.Join(t.TempDir(), "fifo")
				if err := syscall.Mkfifo(to, 0o600); err != nil {
					t.Skipf("cannot make a FIFO here: %v", err)
				}
			}
			w := filepath.Join(t.TempDir(), "w")
			gitIn(t, filepath.Dir(w), "clone", "--quiet", remote, w)
			const linked = "host-0000000000000000.jsonl"
			if err := os.Symlink(to, filepath.Join(w, linked)); err != nil {
				t.Fatal(err)
			}
			gitIn(t, w, "add", linked)
			gitIn(t, w, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "a link")
			gitIn(t, w, "push", "--quiet", "origin", "main")
			if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Lstat(filepath.Join(dir, linked)); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the sync checked out %v (%v); want the link, as git does", info, err)
			}
			var stdout, stderr string
			returnsWithin(t, 30*time.Second, "where with a host file linked to "+target, func() {
				stdout, stderr, _ = exec(t, "where", "--dir", dir)
			})
			if !strings.Contains(stdout, "this machine's own record") || !strings.Contains(stderr, "symbolic link") ||
				!strings.Contains(stderr, linked) {
				t.Fatalf("where said:\n%s\n%s\nwant this machine's record, and the link named", stdout, stderr)
			}
		})
	}
}

// This machine's own journal file, which every record appends to, every sync
// publishes from, init --reclaim compares with the remote's and a hook reads the
// end of, is refused when another account has made it a FIFO, never waited on:
// a record fails, saying so, and a sync, where, init --reclaim and a hook return.
func TestAFIFOAtThisMachinesJournalFileHoldsNothingUp(t *testing.T) {
	hookEnv(t)
	remote := emptyJournalRemote(t)
	root := t.TempDir()
	t.Setenv("COMMS_CHANNELS", root)
	dir := filepath.Join(root, "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "a note the remote holds"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "sync", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, journal.FileName(hostID(t, "--dir", dir))+".jsonl")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(own, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	var err error
	returnsWithin(t, 30*time.Second, "a record into a FIFO", func() {
		_, _, err = exec(t, "record", "--dir", dir, "--type", "note", "--note", "a note into a FIFO")
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("record: %v; want it refused, the file being no regular file", err)
	}
	returnsWithin(t, 30*time.Second, "a sync with a FIFO for this machine's journal file", func() {
		_, _, _ = exec(t, "sync", "--dir", dir, "--timeout", "10s")
	})
	returnsWithin(t, 30*time.Second, "where with a FIFO for this machine's journal file", func() {
		_, _, _ = exec(t, "where", "--dir", dir)
	})
	returnsWithin(t, 30*time.Second, "init --reclaim with a FIFO for this machine's journal file", func() {
		_, _, _ = exec(t, "init", "--reclaim", "--dir", dir, remote)
	})
	feedStdin(t, docClaudeStop)
	returnsWithin(t, 30*time.Second, "a hook with a FIFO for this machine's journal file", func() {
		_, _, err = exec(t, "hook", "claude", "--dir", dir)
	})
	if err != nil {
		t.Fatalf("the hook returned %v; a hook must exit 0 whatever happens", err)
	}
}

// fleetd writes its notes in the clone's git directory through a file of its own
// renamed into place: a FIFO another account planted at the name a note's
// temporary file once had, or at the look's note, holds nothing up.
func TestAFIFOWhereANoteIsWrittenHoldsNothingUp(t *testing.T) {
	t.Parallel()
	for _, note := range []string{"the salt cache's", "the sync's outcome's", "the re-filed sizes'", "the look's"} {
		t.Run(note, func(t *testing.T) {
			t.Parallel()
			remote := emptyJournalRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			if note == "the look's" {
				// A clone of a repository that holds no journal yet: init starts it,
				// noting the look that follows its push.
				w := filepath.Join(t.TempDir(), "w")
				gitIn(t, filepath.Dir(w), "clone", "--quiet", remote, w)
				if err := os.WriteFile(filepath.Join(w, "README.md"), []byte("about\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitIn(t, w, "add", "README.md")
				gitIn(t, w, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "about")
				gitIn(t, w, "push", "--quiet", "origin", "main")
				gitIn(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
				if err := syscall.Mkfifo(filepath.Join(dir, ".git", "fleetd-look"), 0o600); err != nil {
					t.Skipf("cannot make a FIFO here: %v", err)
				}
				var err error
				returnsWithin(t, 30*time.Second, "init with a FIFO at the look's note", func() {
					_, _, err = exec(t, "init", "--dir", dir, "--salt", "s", remote)
				})
				if err != nil {
					t.Fatalf("init: %v", err)
				}
				return
			}
			if note == "the re-filed sizes'" {
				// A record written before init, under another id, which init moves aside
				// and files under the fleet's.
				if _, _, err := exec(t, "record", "--dir", dir, "--type", "note", "--note", "before the salt was known"); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
				t.Fatal(err)
			}
			git := filepath.Join(dir, ".git")
			var fifo string
			switch note {
			case "the salt cache's":
				if err := os.Remove(filepath.Join(git, saltCacheName)); err != nil {
					t.Fatal(err)
				}
				fifo = filepath.Join(git, saltCacheName+".tmp")
			case "the sync's outcome's":
				fifo = filepath.Join(git, syncStatusFile+".tmp")
			case "the re-filed sizes'":
				// The moved copy grows, as when a process appended to it after it
				// moved, so the next sync reads it again and notes its size.
				moved, _ := filepath.Glob(filepath.Join(git, preInitDir, "host-*.jsonl.*"))
				if len(moved) != 1 {
					t.Fatalf("moved copies: %v, want one", moved)
				}
				f, err := os.OpenFile(moved[0], os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("\n")
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					t.Fatal(err)
				}
				fleetFileAged(t, dir)
				fifo = filepath.Join(git, preInitDir, refiledName+".tmp")
			}
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("cannot make a FIFO here: %v", err)
			}
			var err error
			returnsWithin(t, 30*time.Second, "a sync with a FIFO at "+note+" temporary file", func() {
				_, _, err = exec(t, "sync", "--dir", dir, "--timeout", "10s")
			})
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if note == "the salt cache's" && cachedSalt(dir) != "s" {
				t.Fatalf("the salt cache holds %q, want s", cachedSalt(dir))
			}
		})
	}
}
