package gitsync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sleeperEnv, when set, turns this test binary into the sleeper: a process that
// writes its own process id to the named file and then waits far longer than
// any test. The test binary is used rather than a shell's sleep because its id
// is the operating system's: on Windows, the id sh reports for a background job
// is its own, not Windows'.
const sleeperEnv = "GITSYNC_TEST_SLEEPER_PIDFILE"

func TestSleeperHelper(t *testing.T) {
	pidfile := os.Getenv(sleeperEnv)
	if pidfile == "" {
		return // an ordinary test run: nothing to do
	}
	tmp := pidfile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600); err == nil {
		_ = os.Rename(tmp, pidfile)
	}
	time.Sleep(2 * time.Minute)
	os.Exit(0)
}

func TestCancellingASyncKillsEveryProcessGitStarted(t *testing.T) {
	s, err := syncUntilSleeperRuns(t, "sleep 60; true")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation, got %v", err)
	}
	// Termination is asynchronous; give it a moment, but far less than the
	// sleeper's own two minutes.
	deadline := time.Now().Add(10 * time.Second)
	for !processGone(t, s.pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d, started by git's ssh command, outlived the cancelled sync", s.pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.gone = true
}

// sleeper is the process a stand-in ssh started.
type sleeper struct {
	pid  int
	gone bool // proven ended, so its id may now belong to another process
}

// syncUntilSleeperRuns syncs against a remote whose stand-in ssh starts the
// sleeper in the background and then runs sshTail, and cancels the sync as soon
// as the sleeper is running. It returns the sleeper and what Sync returned. The
// sleeper is a child of sh, which is a child of git, so only a kill of git's
// whole tree reaches it. Unless the caller marks it gone, it is killed when the
// test ends.
func syncUntilSleeperRuns(t *testing.T, sshTail string) (*sleeper, error) {
	t.Helper()
	_, m := fleet(t, 1)
	a := m[0]
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "sleeper.pid")
	t.Setenv(sleeperEnv, pidfile)
	// sh accepts a forward-slashed Windows path.
	run(t, a, "remote", "set-url", "origin", "ssh://git@example.invalid/journal.git")
	t.Setenv("GIT_SSH_COMMAND", "'"+filepath.ToSlash(exe)+"' -test.run='^TestSleeperHelper$' & "+sshTail)
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:1"}`)

	// Cancel as soon as the sleeper is running, so the test does not depend on
	// how quickly the machine starts processes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := make(chan int, 1)
	go func() {
		for ctx.Err() == nil {
			if data, err := os.ReadFile(pidfile); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					pid <- n
					cancel()
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	_, err = Sync(ctx, options(a, "host-a"))
	s := &sleeper{}
	select {
	case s.pid = <-pid:
	default:
		t.Fatalf("the stand-in ssh never started the sleeper; sync returned %v", err)
	}
	// Do not leave the sleeper running. Once it is proven gone its id may
	// belong to another process, so it is not killed then.
	t.Cleanup(func() {
		if !s.gone {
			killSleeper(t, s.pid)
		}
	})
	return s, err
}

// When the context ends and something else also went wrong, such as a git that
// could not be killed and was left behind, the error says both, so it does not
// read as a plain timeout. A plain timeout still reads as one.
func TestATimeoutDoesNotHideWhatElseWentWrong(t *testing.T) {
	saved := runGitTree
	t.Cleanup(func() { runGitTree = saved })
	stuck := errors.New("git could not be killed")
	for _, tc := range []struct {
		name string
		err  error // what running git returns after the context ends
		want string
	}{
		{"something else", stuck, "git version: context canceled: git could not be killed"},
		{"only the timeout", context.Canceled, "git version: context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runGitTree = func(func() *exec.Cmd) error {
				cancel()
				return tc.err
			}
			_, err := Git(ctx, t.TempDir(), nil, "version")
			if !errors.Is(err, context.Canceled) || !errors.Is(err, tc.err) || err.Error() != tc.want {
				t.Fatalf("err = %q, want %q wrapping both errors", err, tc.want)
			}
		})
	}
}
