package main

import (
	"bytes"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
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
