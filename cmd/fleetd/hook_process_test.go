package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets a test run this package's own binary as fleetd, so a hook can be
// run the way a tool runs it: a separate process with its event on a real pipe,
// judged by its exit code.
func TestMain(m *testing.M) {
	if os.Getenv("FLEETD_TEST_RUN_MAIN") == "1" {
		if os.Getenv("FLEETD_TEST_PANIC_IN_HOOK") == "1" {
			lookupRepo = func(string, time.Duration) (string, string, error) { panic("injected by the test") }
		}
		main()
		os.Exit(0)
	}
	// The tests set journals up and file at once, a hundred times over: waiting
	// out the settle time each time was most of their run. The test of the wait
	// itself puts back the value fleetd runs with.
	productionSettle, refileSettle = refileSettle, 100*time.Millisecond
	// Every test runs git with an empty global config and no system one, as on a
	// fresh machine, and none reaches this machine's home directory, or a salt or
	// journal directory its environment sets. Set once here, for the whole run,
	// so that a test needing nothing else can run in parallel; a test that sets
	// more of the environment runs on its own.
	home, err := os.MkdirTemp("", "fleetd-test-home-")
	if err != nil {
		panic(err)
	}
	empty := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		panic(err)
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": empty, "GIT_CONFIG_NOSYSTEM": "1", "HOME": home, "USERPROFILE": home} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	for _, k := range []string{"FLEET_SALT", "COMMS_CHANNELS", "GROK_HOOK_EVENT", "CURSOR_VERSION"} {
		if err := os.Unsetenv(k); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	// Tests running in parallel share that configuration: one that wrote it
	// would change what git does in every other.
	if data, err := os.ReadFile(empty); err != nil || len(data) != 0 {
		fmt.Fprintf(os.Stderr, "a test changed the global git configuration every test shares: %q (%v)\n", data, err)
		code = 1
	}
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// productionSettle is refileSettle as fleetd runs with it.
var productionSettle time.Duration

// runFleetd runs fleetd as a process with stdin on a pipe and returns what a tool
// would see.
func runFleetd(t *testing.T, stdin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := osexec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), "FLEETD_TEST_RUN_MAIN=1"), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		var exit *osexec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return o.String(), e.String(), code
}

func TestHookAsAProcessExitsZeroAndPrintsNothing(t *testing.T) {
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	cwd := workRepo(t, "widget-shop", "main")
	stop := event(t, docClaudeStop, map[string]any{"cwd": cwd})
	hookArgs := []string{"hook", "claude", "--dir", journal, "--salt", "s"}

	// The harness sees a non-zero exit: plain fleetd with no command exits 2.
	if _, _, code := runFleetd(t, "", nil); code != 2 {
		t.Fatalf("fleetd with no command exited %d; the harness is not seeing exit codes", code)
	}

	cases := []struct {
		name, stdin string
		env, args   []string
		wantRecords int
		wantLog     string
	}{
		{"an event on a pipe", stop, nil, hookArgs, 1, ""},
		{"not an event", "not json", nil, hookArgs, 1, "not a JSON object"},
		// An unrecovered panic exits 2, the code that keeps a Claude Code or Grok
		// session working with the panic fed to the model as the reason.
		// Another session's turn, which the once-per-half-hour limit on one
		// session's turns does not skip before the panic is reached.
		{"a panic inside the hook", event(t, docClaudeStop, map[string]any{"cwd": cwd, "session_id": "another-session"}),
			[]string{"FLEETD_TEST_PANIC_IN_HOOK=1"}, hookArgs, 1, "internal error, recovered"},
		{"help", "", nil, []string{"hook", "claude", "-h"}, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := readLog(t, logBeside(journal))
			stdout, stderr, code := runFleetd(t, c.stdin, c.env, c.args...)
			if code != 0 {
				t.Errorf("exit %d; a hook exits 0 whatever happens\nstderr: %s", code, stderr)
			}
			if stdout != "" {
				t.Errorf("printed %q; a hook prints nothing", stdout)
			}
			if n := len(hookRecords(t, journal)); n != c.wantRecords {
				t.Errorf("the journal holds %d records, want %d", n, c.wantRecords)
			}
			added := strings.TrimPrefix(readLog(t, logBeside(journal)), before)
			if (c.wantLog == "") != (added == "") || !strings.Contains(added, c.wantLog) {
				t.Errorf("the log gained %q, want %q", added, c.wantLog)
			}
		})
	}
}

// slowGit puts first on PATH a git that takes five seconds to start, as one on a
// loaded machine, or one a scanner holds up, can.
func slowGit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the git stand-in is a shell script")
	}
	real, err := osexec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nsleep 5\nexec '"+real+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Codex kills its SessionEnd hook after a second unless configured otherwise, and
// the hook gives git half a second to name the repository and branch, so that a
// slow git costs the record its repo and branch, not the record. Not parallel: it
// puts a slow git first on PATH.
func TestACodexSessionEndHookRecordsWithinItsBoundWhateverGitTakesToStart(t *testing.T) {
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	cwd := workRepo(t, "widget-shop", "main")
	slowGit(t)
	start := time.Now()
	_, stderr, code := runFleetd(t, event(t, docCodexSessionEnd, map[string]any{"cwd": cwd}), nil,
		"hook", "codex", "--dir", journal, "--salt", "s")
	took := time.Since(start)
	if records := len(hookRecords(t, journal)); code != 0 || records != 1 || took >= 3*time.Second {
		t.Fatalf("Codex's SessionEnd hook with a git slow to start exited %d after %s with %d records; want one "+
			"record within the half second it gives git, far short of the 5 s git takes to start\nstderr: %s",
			code, took.Round(10*time.Millisecond), records, stderr)
	}
}

// Every git command a sync runs ends at its --timeout, a git slow to start
// included, and so does the sync. Not parallel: it puts a slow git first on PATH.
func TestASyncWithAGitSlowToStartEndsAtItsTimeout(t *testing.T) {
	remote := emptyJournalRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", dir, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	slowGit(t)
	start := time.Now()
	_, stderr, code := runFleetd(t, "", nil, "sync", "--dir", dir, "--timeout", "1s")
	if took := time.Since(start); code == 0 || took >= 3*time.Second {
		t.Fatalf("fleetd sync --timeout 1s with a git slow to start exited %d after %s; want it to fail at its "+
			"timeout, far short of the 5 s git takes to start\nstderr: %s", code, took.Round(10*time.Millisecond), stderr)
	}
}

// A hook that records a turn asks git twice, for the repository and for the
// branch, and nothing more: it gives git half a second. Not parallel: it puts a
// git that logs its arguments first on PATH.
func TestAHookAsksGitOnlyForTheRepositoryAndBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git stand-in is a shell script")
	}
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	cwd := workRepo(t, "widget-shop", "main")
	real, err := osexec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$*\" >> '"+log+"'\nexec '"+real+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, stderr, code := runFleetd(t, event(t, docClaudeStop, map[string]any{"cwd": cwd}), nil,
		"hook", "claude", "--dir", journal, "--salt", "s"); code != 0 || len(hookRecords(t, journal)) != 1 {
		t.Fatalf("the hook exited %d with %d records\nstderr: %s", code, len(hookRecords(t, journal)), stderr)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != 2 || !strings.HasSuffix(got[0], " rev-parse --show-toplevel") || !strings.HasSuffix(got[1], " branch --show-current") {
		t.Fatalf("the hook ran git %d times: %q; want once for the repository and once for the branch", len(got), got)
	}
}
