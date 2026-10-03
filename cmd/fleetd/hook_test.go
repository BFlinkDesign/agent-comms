package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
)

// Each tool's own example events, exactly as its documentation prints them. A
// page that shows a union of values, or no example at all, is noted where the
// event is built from the fields it documents.

// Stop input, verbatim from https://code.claude.com/docs/en/hooks ("a Stop input
// with one in-flight shell task and one recurring cron").
const docClaudeStop = `{
  "session_id": "abc123",
  "transcript_path": "~/.claude/projects/.../00893aaf-19fa-41d2-8238-13269b9b3ca0.jsonl",
  "cwd": "/Users/...",
  "permission_mode": "default",
  "hook_event_name": "Stop",
  "stop_hook_active": true,
  "last_assistant_message": "I've completed the refactoring. Here's a summary...",
  "background_tasks": [
    {
      "id": "task-001",
      "type": "shell",
      "status": "running",
      "description": "tail logs",
      "command": "tail -f /var/log/syslog"
    }
  ],
  "session_crons": [
    {
      "id": "cron-001",
      "schedule": "0 9 * * 1-5",
      "recurring": true,
      "prompt": "check the build"
    }
  ]
}`

// SessionEnd input, verbatim from https://code.claude.com/docs/en/hooks.
const docClaudeSessionEnd = `{
  "session_id": "abc123",
  "transcript_path": "/Users/.../.claude/projects/.../00893aaf-19fa-41d2-8238-13269b9b3ca0.jsonl",
  "cwd": "/Users/...",
  "hook_event_name": "SessionEnd",
  "reason": "other"
}`

// The input every Cursor hook receives, verbatim from https://cursor.com/docs/hooks.
// It is schema-shaped (hook_event_name reads "string"), and it has no cwd:
// workspace_roots is the working directory. The same page gives the stop and
// sessionEnd fields as unions of values, so the tests add one documented value
// of each.
const docCursorCommon = `{
  "conversation_id": "string",
  "generation_id": "string",
  "model": "string",
  "model_id": "string",
  "model_params": [{ "id": "string", "value": "string" }],
  "hook_event_name": "string",
  "cursor_version": "string",
  "workspace_roots": ["<path>"],
  "user_email": "string | null",
  "transcript_path": "string | null"
}`

// SessionEnd input, verbatim from https://learn.chatgpt.com/docs/hooks, where
// https://developers.openai.com/codex/hooks now leads.
const docCodexSessionEnd = `{
  "session_id": "thr_123",
  "transcript_path": "/workspace/.codex/rollout.jsonl",
  "cwd": "/workspace",
  "hook_event_name": "SessionEnd",
  "reason": "other"
}`

// The only example event in Grok Build's hook documentation, a PreToolUse,
// verbatim from
// https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md
// "Every event carries the same common fields", so the tests turn it into a Stop
// or a SessionEnd by setting the fields that page documents for those events.
const docGrokPreToolUse = `{
  "hookEventName": "pre_tool_use",
  "hook_event_name": "PreToolUse",
  "sessionId": "abc-123",
  "cwd": "/Users/you/project",
  "workspaceRoot": "/Users/you/project",
  "permissionMode": "default",
  "toolName": "run_terminal_command",
  "toolInput": { "command": "npm test" },
  "timestamp": "2026-04-14T12:00:00Z"
}`

// Codex's Stop input. https://learn.chatgpt.com/docs/hooks has no Stop example,
// so this carries the common fields and the Stop fields it lists (turn_id,
// stop_hook_active, last_assistant_message), with made-up values.
const codexStop = `{
  "session_id": "thr_123",
  "transcript_path": "/workspace/.codex/rollout.jsonl",
  "cwd": "/workspace",
  "hook_event_name": "Stop",
  "model": "gpt-5-codex",
  "permission_mode": "default",
  "turn_id": "turn_1",
  "stop_hook_active": false,
  "last_assistant_message": "Fixed the failing test."
}`

// Codex's notify event. https://learn.chatgpt.com/docs/config-file/config-advanced
// lists its fields (type, thread-id, turn-id, cwd, input-messages,
// last-assistant-message) without an example, so the values are made up.
const codexNotify = `{
  "type": "agent-turn-complete",
  "thread-id": "thr_123",
  "turn-id": "turn_1",
  "cwd": "/workspace",
  "input-messages": ["fix the failing test"],
  "last-assistant-message": "Fixed the failing test."
}`

// The Stop and SessionEnd fields Grok's page documents, applied to its example.
var (
	grokStop = map[string]any{"hookEventName": "stop", "hook_event_name": "Stop", "reason": "end_turn",
		"stopHookActive": false, "lastAssistantMessage": "Done.", "backgroundTasks": []any{}, "sessionCrons": []any{},
		"toolName": nil, "toolInput": nil}
	grokSessionEnd = map[string]any{"hookEventName": "session_end", "hook_event_name": "SessionEnd",
		"toolName": nil, "toolInput": nil}
)

// hookEnv gives a test a git that reads no personal configuration and an
// environment in which no tool appears to be running the hook.
func hookEnv(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GROK_HOOK_EVENT", "CURSOR_VERSION", "COMMS_CHANNELS", "FLEET_SALT"} {
		t.Setenv(k, "")
	}
	// The journal defaults to the home directory, so no test may reach the real one.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// event returns fixture with the fields in set applied, a nil value deleting
// one, encoded as a tool sends it.
func event(t *testing.T, fixture string, set map[string]any) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fixture), &m); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	for k, v := range set {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// feedStdin makes payload this process's stdin until the test ends, the way a
// tool pipes its event to a hook.
func feedStdin(t *testing.T, payload string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
}

// hook runs `fleetd hook` with payload on stdin and holds it to the part of every
// tool's contract that applies whatever happens: exit 0, since a non-zero exit
// shows an error in the session and exit 2 keeps a Claude Code or Grok session
// from stopping, and nothing on stdout, since Codex fails a Stop hook that prints
// plain text and Cursor submits a stop hook's followup_message.
func hook(t *testing.T, payload string, args ...string) (stderr string) {
	t.Helper()
	feedStdin(t, payload)
	stdout, stderr, err := exec(t, append([]string{"hook"}, args...)...)
	if err != nil {
		t.Fatalf("fleetd hook %s returned %v; a hook must exit 0 whatever happens", strings.Join(args, " "), err)
	}
	if stdout != "" {
		t.Fatalf("fleetd hook %s printed %q; without --json a hook prints nothing", strings.Join(args, " "), stdout)
	}
	return stderr
}

// workRepo is the project a tool is working in: a real repository on branch,
// with the tool's working directory one level down inside it.
func workRepo(t *testing.T, name, branch string) (cwd string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	cwd = filepath.Join(root, "src")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init", "--quiet", "--initial-branch="+branch)
	gitIn(t, root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--allow-empty", "-m", "start")
	return cwd
}

type hookRecord struct {
	Type string         `json:"type"`
	From string         `json:"from"`
	Data map[string]any `json:"data"`
}

// hookRecords reads every record in a journal directory.
func hookRecords(t *testing.T, dir string) []hookRecord {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []hookRecord
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var r hookRecord
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("%s: %v\n%s", e.Name(), err, line)
			}
			out = append(out, r)
		}
	}
	return out
}

// publishedRecords counts the records on the remote's main branch, across every
// journal file. twoMachines puts the remote beside the clones.
func publishedRecords(t *testing.T, clone string) int {
	t.Helper()
	remote := filepath.Join(filepath.Dir(clone), "journal.git")
	out, err := osexec.Command("git", "--git-dir", remote, "ls-tree", "--name-only", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-tree: %v\n%s", err, out)
	}
	n := 0
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		blob, err := osexec.Command("git", "--git-dir", remote, "show", "main:"+name).CombinedOutput()
		if err != nil {
			t.Fatalf("git show: %v\n%s", err, blob)
		}
		n += strings.Count(string(blob), "\n")
	}
	return n
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// logBeside is the hook log for a journal directory.
func logBeside(journalDir string) string {
	return filepath.Join(filepath.Dir(journalDir), "fleetd-hook.log")
}

func TestHookRecordsEachToolsTurnAndSessionEnd(t *testing.T) {
	// One case per tool: its per-turn event, then its session-end event, as it
	// sends them, from a real repository, into a real journal clone with a real
	// remote. A turn records without publishing; a session end records and then
	// publishes the machine's file, except for codex, whose session-end hook may
	// run for at most 3 seconds.
	cases := []struct {
		tool string
		// turnEnv and endEnv are what the tool sets for every hook it runs.
		turnEnv, endEnv     map[string]string
		turn, end           func(t *testing.T, cwd string) string
		turnEvent, endEvent string
		endPublishes        bool
	}{
		{
			tool: "claude",
			turn: func(t *testing.T, cwd string) string {
				return event(t, docClaudeStop, map[string]any{"cwd": cwd})
			},
			end: func(t *testing.T, cwd string) string {
				return event(t, docClaudeSessionEnd, map[string]any{"cwd": cwd})
			},
			turnEvent: "Stop", endEvent: "SessionEnd", endPublishes: true,
		},
		{
			tool:    "cursor",
			turnEnv: map[string]string{"CURSOR_VERSION": "1.7.2"},
			endEnv:  map[string]string{"CURSOR_VERSION": "1.7.2"},
			turn: func(t *testing.T, cwd string) string {
				return event(t, docCursorCommon, map[string]any{"workspace_roots": []string{cwd},
					"hook_event_name": "stop", "status": "completed", "loop_count": 0})
			},
			end: func(t *testing.T, cwd string) string {
				return event(t, docCursorCommon, map[string]any{"workspace_roots": []string{cwd},
					"hook_event_name": "sessionEnd", "session_id": "<unique session identifier>", "reason": "window_close",
					"duration_ms": 45000, "is_background_agent": false, "final_status": "<status string>"})
			},
			turnEvent: "stop", endEvent: "sessionEnd", endPublishes: true,
		},
		{
			tool: "codex",
			turn: func(t *testing.T, cwd string) string {
				return event(t, codexStop, map[string]any{"cwd": cwd})
			},
			end: func(t *testing.T, cwd string) string {
				return event(t, docCodexSessionEnd, map[string]any{"cwd": cwd})
			},
			turnEvent: "Stop", endEvent: "SessionEnd", endPublishes: false,
		},
		{
			tool:    "grok",
			turnEnv: map[string]string{"GROK_HOOK_EVENT": "stop"},
			endEnv:  map[string]string{"GROK_HOOK_EVENT": "session_end"},
			turn: func(t *testing.T, cwd string) string {
				return event(t, docGrokPreToolUse, with(grokStop, "cwd", cwd))
			},
			end: func(t *testing.T, cwd string) string {
				return event(t, docGrokPreToolUse, with(grokSessionEnd, "cwd", cwd))
			},
			turnEvent: "Stop", endEvent: "SessionEnd", endPublishes: true,
		},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			hookEnv(t)
			journal, _ := twoMachines(t)
			cwd := workRepo(t, "widget-shop", "feature/hooks")
			want := func(r hookRecord, typ, ev string) {
				t.Helper()
				if r.From != c.tool || r.Type != typ {
					t.Errorf("from %q type %q, want from %q type %s", r.From, r.Type, c.tool, typ)
				}
				if r.Data["note"] != c.tool+" "+ev {
					t.Errorf("note %v, want %q", r.Data["note"], c.tool+" "+ev)
				}
				if r.Data["repo"] != "widget-shop" || r.Data["branch"] != "feature/hooks" {
					t.Errorf("repo %v branch %v, want widget-shop on feature/hooks, from the repository at the event's working directory",
						r.Data["repo"], r.Data["branch"])
				}
				if r.Data["host.id"] == nil || r.Data["host.name"] == nil {
					t.Errorf("the record carries no host attribution: %+v", r.Data)
				}
			}

			for k, v := range c.turnEnv {
				t.Setenv(k, v)
			}
			hook(t, c.turn(t, cwd), c.tool, "--dir", journal, "--salt", "s")
			recs := hookRecords(t, journal)
			if len(recs) != 1 {
				t.Fatalf("after a turn the journal holds %d records, want 1: %+v", len(recs), recs)
			}
			want(recs[0], "turn", c.turnEvent)
			if n := publishedRecords(t, journal); n != 0 {
				t.Errorf("a turn published %d records; a turn ends after every reply and must not push", n)
			}

			for k, v := range c.endEnv {
				t.Setenv(k, v)
			}
			hook(t, c.end(t, cwd), c.tool, "--dir", journal, "--salt", "s")
			recs = hookRecords(t, journal)
			if len(recs) != 2 {
				t.Fatalf("after the session ended the journal holds %d records, want 2: %+v", len(recs), recs)
			}
			want(recs[1], "session", c.endEvent)
			published := publishedRecords(t, journal)
			if c.endPublishes && published != 2 {
				t.Errorf("the remote holds %d records after the session ended, want the turn's and the session's", published)
			}
			if !c.endPublishes && published != 0 {
				t.Errorf("the remote holds %d records; %s's session end must not sync", published, c.tool)
			}
			if log := readLog(t, logBeside(journal)); log != "" {
				t.Errorf("nothing went wrong, but the hook logged:\n%s", log)
			}
		})
	}
}

// with returns a copy of set with one more field.
func with(set map[string]any, key string, value any) map[string]any {
	out := map[string]any{}
	for k, v := range set {
		out[k] = v
	}
	out[key] = value
	return out
}

func TestHookAcceptsEachDocumentedExampleAsPrinted(t *testing.T) {
	// The examples exactly as the documentation prints them, byte for byte. Their
	// working directories are placeholders that do not exist here, so each is
	// recorded without repo or branch, and the log says why.
	cases := []struct {
		name, tool, payload, env, wantType, placeholder string
	}{
		{"claude Stop", "claude", docClaudeStop, "", "turn", "/Users/..."},
		{"claude SessionEnd", "claude", docClaudeSessionEnd, "", "session", "/Users/..."},
		{"cursor common input", "cursor", docCursorCommon, "CURSOR_VERSION", "hook", "<path>"},
		{"codex SessionEnd", "codex", docCodexSessionEnd, "", "session", "/workspace"},
		{"grok PreToolUse", "grok", docGrokPreToolUse, "GROK_HOOK_EVENT", "hook", "/Users/you/project"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hookEnv(t)
			if c.env != "" {
				t.Setenv(c.env, "set by the tool")
			}
			journal := filepath.Join(t.TempDir(), "journal")
			hook(t, c.payload, c.tool, "--dir", journal, "--salt", "s", "--no-sync")
			recs := hookRecords(t, journal)
			if len(recs) != 1 || recs[0].Type != c.wantType || recs[0].From != c.tool {
				t.Fatalf("records %+v, want one %s record from %s", recs, c.wantType, c.tool)
			}
			if fi, err := os.Stat(c.placeholder); err == nil && fi.IsDir() {
				t.Skipf("the documented placeholder %s is a real directory here", c.placeholder)
			}
			if _, ok := recs[0].Data["repo"]; ok {
				t.Errorf("recorded repo %v for a working directory that does not exist", recs[0].Data["repo"])
			}
			if log := readLog(t, logBeside(journal)); !strings.Contains(log, "not a directory on this machine") {
				t.Errorf("the log does not say why repo and branch are missing:\n%s", log)
			}
		})
	}
}

func TestHookCodexNotifyTakesTheEventAsTheLastArgument(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	cwd := workRepo(t, "widget-shop", "main")
	// notify passes the event as an argument after the configured ones, so stdin
	// holds nothing.
	hook(t, "", "codex", "--dir", journal, "--salt", "s", event(t, codexNotify, map[string]any{"cwd": cwd}))

	recs := hookRecords(t, journal)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if r := recs[0]; r.From != "codex" || r.Type != "turn" || r.Data["note"] != "codex agent-turn-complete" ||
		r.Data["repo"] != "widget-shop" || r.Data["branch"] != "main" {
		t.Errorf("record %+v: want a codex turn in widget-shop on main", r)
	}
	// Codex starts its notify program and neither waits for it nor limits it,
	// so a turn it records is published at once: a machine that runs only Codex
	// still publishes.
	if n := publishedRecords(t, journal); n != 1 {
		t.Errorf("the remote holds %d records, want the notify turn's", n)
	}
}

func TestHookLeavesAnotherToolsEventToThatToolsOwnHook(t *testing.T) {
	// Grok runs Claude Code's and Cursor's user hooks, and Cursor can run Claude
	// Code's, so a configured hook also fires inside the other tools' sessions.
	// Recording those events under the configured tool's name would misattribute
	// them, and under the host's would duplicate its own hook's records. Cursor
	// is told apart by the CURSOR_VERSION it sets for every hook, since what it
	// sends Claude Code's hooks is not documented.
	cases := []struct {
		name, tool, env, payload string
		wantRecorded             bool
	}{
		{"cursor's own event, claude's hook", "claude", "",
			`{"hook_event_name": "stop", "cursor_version": "1.7.2", "workspace_roots": []}`, false},
		{"a claude-shaped event under cursor", "claude", "CURSOR_VERSION", docClaudeStop, false},
		{"grok's event, claude's hook", "claude", "", event(t, docGrokPreToolUse, grokStop), false},
		{"a claude-shaped event under grok", "claude", "GROK_HOOK_EVENT", docClaudeStop, false},
		{"grok's event, cursor's hook", "cursor", "GROK_HOOK_EVENT", event(t, docGrokPreToolUse, grokSessionEnd), false},
		{"grok under cursor's name too", "cursor", "CURSOR_VERSION", event(t, docGrokPreToolUse, grokStop), false},
		{"claude's own event", "claude", "", docClaudeStop, true},
		{"cursor's own event", "cursor", "CURSOR_VERSION", event(t, docCursorCommon, map[string]any{"hook_event_name": "stop"}), true},
		{"grok's own event", "grok", "GROK_HOOK_EVENT", event(t, docGrokPreToolUse, grokStop), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hookEnv(t)
			if c.env != "" {
				t.Setenv(c.env, "set by the tool")
			}
			journal := filepath.Join(t.TempDir(), "journal")
			hook(t, c.payload, c.tool, "--dir", journal, "--salt", "s")
			if got := len(hookRecords(t, journal)) == 1; got != c.wantRecorded {
				t.Errorf("recorded = %v, want %v", got, c.wantRecorded)
			}
		})
	}

	// --json, for a person trying a hook by hand, says why.
	hookEnv(t)
	t.Setenv("CURSOR_VERSION", "1.7.2")
	feedStdin(t, docClaudeStop)
	stdout, _, err := exec(t, "hook", "claude", "--dir", filepath.Join(t.TempDir(), "journal"), "--salt", "s", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Recorded bool   `json:"recorded"`
		Skipped  string `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("hook --json: %v\n%s", err, stdout)
	}
	if res.Recorded || !strings.Contains(res.Skipped, "fleetd hook cursor") {
		t.Errorf("hook --json = %s, want it skipped with a pointer to fleetd hook cursor", stdout)
	}
}

func TestHookGrokSessionEndStopAndSubagentEventsAreNotRecorded(t *testing.T) {
	// Grok fires one more Stop when a session ends, with reason channel_closed or
	// shutdown, and marks a subagent's events with subagentType; its page tells a
	// hook to leave both out.
	hookEnv(t)
	t.Setenv("GROK_HOOK_EVENT", "stop")
	journal := filepath.Join(t.TempDir(), "journal")
	grok := func(set map[string]any) {
		t.Helper()
		hook(t, event(t, docGrokPreToolUse, set), "grok", "--dir", journal, "--salt", "s", "--no-sync")
	}
	grok(with(grokStop, "reason", "shutdown"))
	grok(with(grokStop, "reason", "channel_closed"))
	grok(with(grokStop, "subagentType", "explore"))
	grok(with(grokSessionEnd, "subagentType", "explore"))
	if recs := hookRecords(t, journal); len(recs) != 0 {
		t.Fatalf("recorded Grok's session-end Stop or a subagent's event: %+v", recs)
	}
	grok(grokStop)
	if recs := hookRecords(t, journal); len(recs) != 1 {
		t.Fatalf("got %d records for one real turn, want 1", len(recs))
	}
}

func TestHookRepoAndBranchComeFromTheEventsWorkingDirectory(t *testing.T) {
	hookEnv(t)
	gitCommit := []string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--allow-empty", "-m", "start"}

	detached := workRepo(t, "detached-shop", "main")
	gitIn(t, filepath.Dir(detached), "checkout", "--quiet", "--detach")

	// A linked worktree is named by its own directory, as the toplevel is.
	linked := filepath.Join(t.TempDir(), "widget-shop-hotfix")
	gitIn(t, filepath.Dir(workRepo(t, "widget-shop", "main")), "worktree", "add", "--quiet", "-b", "hotfix", linked)

	// A repository git cannot read: its HEAD is not a ref.
	broken := workRepo(t, "broken-shop", "main")
	if err := os.WriteFile(filepath.Join(filepath.Dir(broken), ".git", "HEAD"), []byte("not a ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A home directory kept in git is named after the account, and records are
	// committed.
	home := filepath.Join(t.TempDir(), "brady")
	homeNotes := filepath.Join(home, "notes")
	if err := os.MkdirAll(homeNotes, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, home, "init", "--quiet", "--initial-branch=main")
	gitIn(t, home, gitCommit...)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cases := []struct {
		name               string
		set                map[string]any
		wantRepo, wantBrch string
		wantLog            string
	}{
		{name: "no working directory", set: map[string]any{"cwd": nil}},
		{name: "not a repository", set: map[string]any{"cwd": t.TempDir()}},
		{name: "a directory that is gone", set: map[string]any{"cwd": filepath.Join(t.TempDir(), "gone")},
			wantLog: "not a directory on this machine"},
		{name: "detached HEAD", set: map[string]any{"cwd": detached}, wantRepo: "detached-shop"},
		{name: "linked worktree", set: map[string]any{"cwd": linked}, wantRepo: "widget-shop-hotfix", wantBrch: "hotfix"},
		{name: "a repository git cannot read", set: map[string]any{"cwd": broken}, wantLog: "could not read the repository"},
		{name: "the home directory", set: map[string]any{"cwd": homeNotes}},
		{name: "fields no tool documents", wantRepo: "widget-shop", wantBrch: "main",
			set: map[string]any{"cwd": workRepo(t, "widget-shop", "main"),
				"a_future_field": map[string]any{"nested": []any{1, "x", nil}}, "hook_event_name": 7}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "journal")
			hook(t, event(t, docClaudeStop, c.set), "claude", "--dir", journal, "--salt", "s")
			recs := hookRecords(t, journal)
			if len(recs) != 1 {
				t.Fatalf("got %d records, want 1: an event is recorded whatever its working directory", len(recs))
			}
			repo, _ := recs[0].Data["repo"].(string)
			branch, _ := recs[0].Data["branch"].(string)
			if repo != c.wantRepo || branch != c.wantBrch {
				t.Errorf("repo %q branch %q, want %q and %q", repo, branch, c.wantRepo, c.wantBrch)
			}
			log := readLog(t, logBeside(journal))
			if c.wantLog == "" && log != "" {
				t.Errorf("nothing went wrong, but the hook logged:\n%s", log)
			}
			if !strings.Contains(log, c.wantLog) {
				t.Errorf("the log does not say %q:\n%s", c.wantLog, log)
			}
		})
	}
}

func TestHookNeverFailsTheToolAndLogsWhy(t *testing.T) {
	hookEnv(t)
	root := t.TempDir()
	t.Setenv("COMMS_CHANNELS", root)
	journal := filepath.Join(root, "journal") // not a clone, so a sync fails
	logPath := filepath.Join(root, "fleetd-hook.log")
	sessionEnd := event(t, docClaudeSessionEnd, map[string]any{"cwd": nil})
	stop := event(t, docClaudeStop, map[string]any{"cwd": nil})

	cases := []struct {
		name, payload string
		args          []string
		wantLog       string
		wantRecords   int
	}{
		{"the event is not JSON", "not json", []string{"claude", "--salt", "s"}, "not a JSON object", 0},
		{"the event is JSON null", "null", []string{"claude", "--salt", "s"}, "JSON null", 0},
		{"the event is not an object", "[]", []string{"claude", "--salt", "s"}, "not a JSON object", 0},
		{"there is no event", "", []string{"claude", "--salt", "s"}, "stdin was empty", 0},
		{"the sync fails", sessionEnd, []string{"claude", "--salt", "s"}, "sync:", 1},
		{"an unknown flag", stop, []string{"claude", "--bogus"}, "bogus", 0},
		{"a timeout that is not positive", stop, []string{"claude", "--salt", "s", "--timeout", "0s"}, "--timeout must be positive", 0},
		{"an unknown tool", stop, []string{"claud", "--salt", "s"}, `unknown tool "claud"`, 0},
		{"no tool", stop, nil, `unknown tool ""`, 0},
		{"no salt", stop, []string{"claude"}, "FLEET_SALT", 1},
	}
	records := 0
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FLEET_SALT", "")
			before := readLog(t, logPath)
			hook(t, c.payload, c.args...)
			added := strings.TrimPrefix(readLog(t, logPath), before)
			if !strings.Contains(added, c.wantLog) {
				t.Errorf("the log gained %q, want it to say %q", added, c.wantLog)
			}
			records += c.wantRecords
			if got := len(hookRecords(t, journal)); got != records {
				t.Errorf("the journal holds %d records, want %d", got, records)
			}
		})
	}
}

func TestHookStillRecordsWhenGivenAStrayArgument(t *testing.T) {
	// A configuration that also passes the event's name, as some examples do,
	// is a mistake worth logging, not a reason to lose the event.
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			hookEnv(t)
			journal := filepath.Join(t.TempDir(), "journal")
			hook(t, event(t, docClaudeStop, map[string]any{"cwd": nil}), tool, "--dir", journal, "--salt", "s", "Stop")
			if n := len(hookRecords(t, journal)); n != 1 {
				t.Errorf("got %d records, want the event on stdin recorded", n)
			}
			if log := readLog(t, logBeside(journal)); !strings.Contains(log, `ignored unexpected arguments ["Stop"]`) {
				t.Errorf("the log does not name the stray argument:\n%s", log)
			}
		})
	}
}

func TestHookHelpIsNotAnExitCodeThatBlocksAStop(t *testing.T) {
	// main exits 2 for help, and exit 2 from a Stop hook keeps a Claude Code or
	// Grok session from stopping.
	hookEnv(t)
	t.Setenv("COMMS_CHANNELS", t.TempDir())
	for _, args := range [][]string{{"hook", "-h"}, {"hook", "help"}, {"hook", "claude", "-h"}, {"hook", "claude", "--help"}} {
		feedStdin(t, "")
		_, stderr, err := exec(t, args...)
		if err != nil {
			t.Errorf("fleetd %s returned %v; a hook exits 0", strings.Join(args, " "), err)
		}
		if !strings.Contains(stderr, "usage:") || !strings.Contains(stderr, "fleetd hook") {
			t.Errorf("fleetd %s did not show usage on stderr: %q", strings.Join(args, " "), stderr)
		}
	}
}

func TestHookReadsAnEventThatPowerShellPrefixedWithAByteOrderMark(t *testing.T) {
	// On Windows the tools run a hook command through a shell, usually
	// PowerShell, which re-encodes what it pipes to a program and can prefix a
	// byte order mark. JSON never starts with one.
	hookEnv(t)
	cwd := workRepo(t, "widget-shop", "main")
	plain := event(t, docClaudeStop, map[string]any{"cwd": cwd})
	utf16Of := func(order binary.AppendByteOrder, bom []byte) string {
		out := append([]byte{}, bom...)
		for _, u := range utf16.Encode([]rune(plain)) {
			out = order.AppendUint16(out, u)
		}
		return string(out)
	}
	for name, payload := range map[string]string{
		"UTF-8 with a mark":  "\xEF\xBB\xBF" + plain,
		"UTF-16 LE":          utf16Of(binary.LittleEndian, []byte{0xFF, 0xFE}),
		"UTF-16 BE":          utf16Of(binary.BigEndian, []byte{0xFE, 0xFF}),
		"UTF-8 without mark": plain,
	} {
		t.Run(name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "journal")
			hook(t, payload, "claude", "--dir", journal, "--salt", "s")
			recs := hookRecords(t, journal)
			if len(recs) != 1 || recs[0].Data["repo"] != "widget-shop" || recs[0].Data["note"] != "claude Stop" {
				t.Fatalf("records %+v, log:\n%s", recs, readLog(t, logBeside(journal)))
			}
		})
	}
}

func TestHookWithoutAnAbsoluteJournalDirWritesNothingWhereTheToolRuns(t *testing.T) {
	// A hook runs in whatever directory the tool chose, usually the project, so a
	// relative journal directory would put a journal in every project worked in.
	// With neither --dir nor COMMS_CHANNELS the journal is the home directory's.
	hookEnv(t)
	project := t.TempDir()
	t.Chdir(project)
	stop := event(t, docClaudeStop, map[string]any{"cwd": project})
	hook(t, stop, "claude", "--salt", "s")
	if n := len(hookRecords(t, filepath.Join(os.Getenv("HOME"), ".ai", "channels", "journal"))); n != 1 {
		t.Errorf("the home directory's journal holds %d records, want the hook's", n)
	}
	if stderr := hook(t, stop, "claude", "--salt", "s", "--dir", filepath.Join("channels", "journal")); !strings.Contains(stderr, "relative") {
		t.Errorf("stderr = %q, want it to refuse a relative journal directory", stderr)
	}
	t.Setenv("COMMS_CHANNELS", "channels")
	if stderr := hook(t, stop, "claude", "--salt", "s"); !strings.Contains(stderr, "relative") {
		t.Errorf("stderr = %q, want it to refuse a relative COMMS_CHANNELS", stderr)
	}
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the hook wrote into the tool's working directory: %v", entries)
	}
}

func TestHookNoSyncLeavesTheSessionForTheNextSync(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	sessionEnd := event(t, docClaudeSessionEnd, map[string]any{"cwd": workRepo(t, "widget-shop", "main")})
	hook(t, sessionEnd, "claude", "--dir", journal, "--salt", "s", "--no-sync")
	if n := publishedRecords(t, journal); n != 0 {
		t.Fatalf("--no-sync published %d records", n)
	}
	// The next session end publishes the machine's whole file: both records.
	hook(t, sessionEnd, "claude", "--dir", journal, "--salt", "s")
	if n := publishedRecords(t, journal); n != 2 {
		t.Fatalf("the remote holds %d records, want both sessions'", n)
	}
}

func TestHookSyncIsBoundedByTimeout(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	hook(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": nil}),
		"claude", "--dir", journal, "--salt", "s", "--timeout", "1ns")
	if log := readLog(t, logBeside(journal)); !strings.Contains(log, "deadline exceeded") {
		t.Errorf("the log does not say the sync ran out of time:\n%s", log)
	}
	if n := publishedRecords(t, journal); n != 0 {
		t.Fatalf("published %d records in 1ns", n)
	}
	// Nothing was lost: the next sync publishes the hook's record.
	if _, stderr, err := exec(t, "sync", "--dir", journal, "--salt", "s"); err != nil {
		t.Fatalf("sync after a timed-out hook: %v\n%s", err, stderr)
	}
	if n := publishedRecords(t, journal); n != 1 {
		t.Fatalf("the remote holds %d records, want the one the hook made", n)
	}
}

func TestHookJSONSaysWhatItDid(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	feedStdin(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": workRepo(t, "widget-shop", "main")}))
	stdout, _, err := exec(t, "hook", "claude", "--dir", journal, "--salt", "s", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Tool, Event, Type, Repo, Branch, ID, Log string
		Recorded, Synced                         bool
		Sync                                     struct{ Published int }
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if res.Tool != "claude" || res.Event != "SessionEnd" || res.Type != "session" || !res.Recorded ||
		res.Repo != "widget-shop" || res.Branch != "main" || !strings.HasPrefix(res.ID, "hive:") ||
		!res.Synced || res.Sync.Published != 1 || res.Log != logBeside(journal) {
		t.Errorf("hook --json = %s", stdout)
	}
}

func TestHookLogIsKeptToTwoBoundedFiles(t *testing.T) {
	hookEnv(t)
	root := t.TempDir()
	t.Setenv("COMMS_CHANNELS", root)
	logPath := filepath.Join(root, "fleetd-hook.log")
	for i, old := range []string{strings.Repeat("an old problem\n", 20000), strings.Repeat("a newer problem\n", 20000)} {
		if err := os.WriteFile(logPath, []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		hook(t, "not json", "claude", "--salt", "s")
		if got := readLog(t, logPath+".1"); got != old {
			t.Fatalf("round %d: the full log was not moved aside intact (%d bytes kept of %d)", i, len(got), len(old))
		}
		if got := readLog(t, logPath); !strings.Contains(got, "not a JSON object") || len(got) > 1024 {
			t.Fatalf("round %d: the new log holds %d bytes: %q", i, len(got), got)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fleetd-hook.log") {
			logs = append(logs, e.Name())
		}
	}
	if len(logs) != 2 {
		t.Errorf("log files %v, want the log and one older copy", logs)
	}
}

// A tool runs its Stop hook after every reply. One turn record per session per
// half hour says where the work happened as well as one per reply would, and
// keeps each machine's journal, which every machine reads, small.
func TestHookRecordsASessionsTurnsAtMostOnceEveryHalfHour(t *testing.T) {
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	cwd := workRepo(t, "widget-shop", "main")
	turn := func(session string) {
		t.Helper()
		hook(t, event(t, docClaudeStop, map[string]any{"cwd": cwd, "session_id": session}), "claude", "--dir", journal, "--salt", "s")
	}
	turn("one")
	turn("one")
	turn("one")
	turn("two")
	recs := hookRecords(t, journal)
	if len(recs) != 2 || recs[0].Data["session"] != "one" || recs[1].Data["session"] != "two" {
		t.Fatalf("got %+v, want one turn of each session", recs)
	}
	// Half an hour later, session one's next turn is recorded again.
	file := filepath.Join(journal, strings.ReplaceAll(hostID(t, "--salt", "s", "--dir", t.TempDir()), ":", "-")+".jsonl")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339Nano)
	var aged []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		rec["ts"] = old
		b, _ := json.Marshal(rec)
		aged = append(aged, string(b))
	}
	if err := os.WriteFile(file, []byte(strings.Join(aged, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn("one")
	if n := len(hookRecords(t, journal)); n != 3 {
		t.Fatalf("got %d records, want session one's turn recorded again after half an hour", n)
	}
	// A session's end is always recorded, whatever its turns were.
	hook(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": cwd, "session_id": "one"}), "claude", "--dir", journal, "--salt", "s", "--no-sync")
	if n := len(hookRecords(t, journal)); n != 4 {
		t.Fatalf("got %d records, want the session end too", n)
	}
}

// A hook started without FLEET_SALT, as one a tool launched before the variable
// was set runs, takes the salt from the journal's fleetd.json: its records are
// the machine's, and nothing is logged.
func TestHookTakesTheSaltFromTheJournal(t *testing.T) {
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(journal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journal, gitsync.FleetFile), []byte(`{"salt": "the-fleets"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": nil}), "claude", "--dir", journal, "--no-sync")
	want := strings.ReplaceAll(hostID(t, "--salt", "the-fleets", "--dir", t.TempDir()), ":", "-") + ".jsonl"
	if _, err := os.Stat(filepath.Join(journal, want)); err != nil {
		t.Fatalf("the record is not in the fleet's file %s: %v", want, err)
	}
	if log := readLog(t, logBeside(journal)); log != "" {
		t.Fatalf("the hook logged %q", log)
	}
}

// Two tools ending sessions at once both sync; the second finds the first's
// lock. That is not a problem: the next sync publishes its record.
func TestHookLeavesARecordToTheNextSyncWhenAnotherSyncIsRunning(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	if err := os.WriteFile(filepath.Join(journal, ".git", "fleetd-sync.lock"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": nil}), "claude", "--dir", journal, "--salt", "s")
	if log := readLog(t, logBeside(journal)); log != "" {
		t.Fatalf("the hook logged %q for a sync another sync was already running", log)
	}
	if n := len(hookRecords(t, journal)); n != 1 {
		t.Fatalf("got %d records, want the session end", n)
	}
}

// where says when a hook logged a problem, since a hook's own output goes
// nowhere a person looks.
func TestWhereMentionsARecentHookProblem(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	hook(t, "not json", "claude", "--dir", journal, "--salt", "s")
	hook(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": nil}), "claude", "--dir", journal, "--salt", "s", "--no-sync")
	stdout, _, err := exec(t, "where", "--dir", journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "hook logged a problem") || !strings.Contains(stdout, "not a JSON object") || !strings.Contains(stdout, logBeside(journal)) {
		t.Fatalf("where does not mention the hook's problem:\n%s", stdout)
	}
}

// Cursor on Windows gives a workspace root as "/C:/Users/...", which Windows
// reads as a directory named "C:" on the current drive (cc-safety-net#174 shows
// such a payload from Cursor 3.22.12 on Windows 11).
func TestLocalPathUndoesCursorsWindowsDriveForm(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, goos, want string }{
		{"/C:/Users/someone/git-repos/research", "windows", "C:/Users/someone/git-repos/research"},
		{"/d:/work", "windows", "d:/work"},
		{`C:\Users\someone`, "windows", `C:\Users\someone`},
		{"/C:/Users/someone", "linux", "/C:/Users/someone"},
		{"/home/someone/work", "linux", "/home/someone/work"},
		{"/1:/x", "windows", "/1:/x"},
	} {
		if got := localPath(c.in, c.goos); got != c.want {
			t.Errorf("localPath(%q, %s) = %q, want %q", c.in, c.goos, got, c.want)
		}
	}
}

// A turn written while the clock ran fast is in the future; it must not keep the
// session's later turns from being recorded until the clock catches up.
func TestHookIgnoresATurnDatedInTheFuture(t *testing.T) {
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	cwd := workRepo(t, "widget-shop", "main")
	if _, _, err := exec(t, "record", "--dir", journal, "--salt", "s", "--type", "turn", "--note", "claude Stop",
		"--agent", "claude", "--at", time.Now().Add(6*time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// The record above has no session; give the future turn this session's.
	file := filepath.Join(journal, strings.ReplaceAll(hostID(t, "--salt", "s", "--dir", t.TempDir()), ":", "-")+".jsonl")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec["data"].(map[string]any)["tool"], rec["data"].(map[string]any)["session"] = "claude", "fast-clock"
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	hook(t, event(t, docClaudeStop, map[string]any{"cwd": cwd, "session_id": "fast-clock"}), "claude", "--dir", journal, "--salt", "s")
	if n := len(hookRecords(t, journal)); n != 2 {
		t.Fatalf("got %d records, want this turn recorded despite the one dated in the future", n)
	}
}

// Codex gives its own SessionEnd hook 1 second unless configured otherwise, and
// at most 3, then kills it: its hooks ask git for at most half a second, so a
// slow git costs the record its repo and branch, not the record. Its notify
// program, which Codex does not limit, and the other tools keep the full wait.
func TestCodexsOwnHooksBoundTheirGitLookup(t *testing.T) {
	hookEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	var waited []time.Duration
	saved := lookupRepo
	t.Cleanup(func() { lookupRepo = saved })
	lookupRepo = func(cwd string, wait time.Duration) (string, string, error) {
		waited = append(waited, wait)
		return "", "", nil
	}
	hook(t, event(t, docCodexSessionEnd, nil), "codex", "--dir", journal, "--salt", "s", "--no-sync")
	hook(t, "", "codex", "--dir", journal, "--salt", "s", "--no-sync", event(t, codexNotify, map[string]any{"thread-id": "another"}))
	hook(t, event(t, docClaudeSessionEnd, nil), "claude", "--dir", journal, "--salt", "s", "--no-sync")
	want := []time.Duration{codexGitWait, hookGitWait, hookGitWait}
	if fmt.Sprint(waited) != fmt.Sprint(want) {
		t.Fatalf("git waits = %v, want %v", waited, want)
	}
}

// With both of Codex's mechanisms configured, its Stop hook records a turn
// (and never syncs) and notify, for the same turn, finds it already recorded.
// notify must still publish it: on a machine that runs only Codex, nothing else
// would until a session ended elsewhere.
func TestNotifyPublishesATurnCodexsStopHookRecorded(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	cwd := workRepo(t, "widget-shop", "main")
	stop := event(t, codexStop, map[string]any{"cwd": cwd, "session_id": "thr_1"})
	hook(t, stop, "codex", "--dir", journal, "--salt", "s")
	if n := publishedRecords(t, journal); n != 0 {
		t.Fatalf("Codex's Stop hook published %d records; its hooks never sync", n)
	}
	hook(t, "", "codex", "--dir", journal, "--salt", "s", event(t, codexNotify, map[string]any{"cwd": cwd, "thread-id": "thr_1"}))
	if n := len(hookRecords(t, journal)); n != 1 {
		t.Fatalf("got %d records, want the one turn, recorded once", n)
	}
	if n := publishedRecords(t, journal); n != 1 {
		t.Fatalf("the remote holds %d records, want the turn notify found recorded", n)
	}
}

// The first machine's hooks all failing is exactly when where must say so,
// though the journal has nothing in it yet.
func TestWhereShowsAHookProblemWhenThereAreNoRecords(t *testing.T) {
	hookEnv(t)
	journal, _ := twoMachines(t)
	hook(t, "not json", "claude", "--dir", journal, "--salt", "s")
	stdout, _, err := exec(t, "where", "--dir", journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "hook logged a problem") || !strings.Contains(stdout, "no records") {
		t.Fatalf("where with no records does not mention the hook's problem:\n%s", stdout)
	}
}

// A record written while a sync ran, after it read the file, is not published
// by it, however that sync ended. notify must still see it as unsynced.
func TestARecordWrittenDuringASyncCountsAsUnsynced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "host-0123456789abcdef.jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	began, ended := time.Now().Add(-10*time.Second), time.Now().Add(-2*time.Second)
	written := began.Add(3 * time.Second)
	if err := os.Chtimes(file, written, written); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf(`{"last_success": {"at": %q, "started": %q}}`,
		ended.UTC().Format(time.RFC3339), began.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, ".git", "fleetd-sync.json"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	if !unsynced(dir, file) {
		t.Fatal("a record written while the last sync ran counts as synced")
	}
}

// On a machine set up with init, a stale FLEET_SALT is worth a warning, but not
// the advice to run init.
func TestTheHookAdvisesInitOnlyWhenTheJournalHasNoFleetFile(t *testing.T) {
	hookEnv(t)
	remote := emptyJournalRemote(t)
	journal := filepath.Join(t.TempDir(), "journal")
	if _, _, err := exec(t, "init", "--dir", journal, "--salt", "s", remote); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SALT", "stale")
	hook(t, event(t, docClaudeSessionEnd, map[string]any{"cwd": t.TempDir()}), "claude", "--dir", journal, "--no-sync")
	log, err := os.ReadFile(logBeside(journal))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "FLEET_SALT differs") || strings.Contains(string(log), "fleetd init") {
		t.Fatalf("hook log:\n%s", log)
	}
}
