package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

// `fleetd hook <tool>` is what an AI tool's own hook system runs when a turn or a
// session ends. It turns the event the tool documents into one journal record,
// through the same code path as `fleetd record`, so which machine did the work
// is recorded without anyone having to remember to.
//
// A hook runs inside someone else's session, and every one of these tools gives
// a hook's exit code and output a meaning. Exit 2 from a Stop hook keeps a Claude
// Code or Grok session working and feeds stderr to the model as the reason;
// Codex treats plain text on a Stop hook's stdout as a failure; Cursor
// auto-submits a followup_message that a stop hook prints; any other non-zero
// exit shows an error in the session. So a hook prints nothing, exits 0 whatever
// happened, even when it panics, and writes any problem to a log beside the
// journal directory instead. A journal that cannot be written must never become
// a session that cannot stop.

const (
	// hookLogName is the file, beside the journal directory, that a hook appends
	// its problems to. Beside rather than inside: the journal directory is a git
	// clone that sync publishes from, and a log has no business in it.
	hookLogName = "fleetd-hook.log"
	// hookLogMax is the size at which the log is moved to hookLogName+".1",
	// replacing the previous one, so at most two files of this size are kept.
	hookLogMax = 256 << 10
	// hookPayloadMax bounds the event read from stdin. Claude Code's Stop event
	// carries the whole last assistant message, so this is generous.
	hookPayloadMax = 8 << 20
	// hookStdinWait bounds how long a hook waits for the tool to close stdin.
	hookStdinWait = 5 * time.Second
	// hookGitWait bounds the two git calls that name the repository and branch,
	// together.
	hookGitWait = 5 * time.Second
	// codexGitWait is hookGitWait for Codex's own hooks: Codex gives a SessionEnd
	// hook 1 second unless configured otherwise, and at most 3, and kills it
	// after, so a slow git costs the record its repo and branch, not the record.
	codexGitWait = 500 * time.Millisecond
	// hookSyncTimeout is the default --timeout. With the waits above, a hook that
	// syncs finishes within about 50 seconds; the configurations in AGENTS.md give
	// a session-end hook 60, so fleetd stops a slow sync and releases its lock
	// itself instead of being killed while holding it.
	hookSyncTimeout = 40 * time.Second
	// hookTurnGap is the least time between two turn records of one session. A
	// turn ends after every reply, and a record every half hour says where the
	// work happened as well as one per reply would, at a fraction of the journal.
	hookTurnGap = 30 * time.Minute
	// hookTailBytes is how much of this machine's journal file is read to find
	// the session's last turn record.
	hookTailBytes = 64 << 10
)

// hookTool is what fleetd knows about one tool's documented hook contract.
type hookTool struct {
	// cwd returns the event's working directory, from the field the tool
	// documents for it.
	cwd func(payload) string
	// argv is set when the tool may pass the event as the last command-line
	// argument instead of on stdin. Such an event is synced when recorded: see
	// hookTools.
	argv bool
	// syncAtSessionEnd says whether the tool's session-end event publishes.
	syncAtSessionEnd bool
}

// hookTools are the tools whose hook mechanism, event payload and exit-code
// contract were checked against their own documentation on 2026-09-25:
//
//	claude  https://code.claude.com/docs/en/hooks
//	cursor  https://cursor.com/docs/hooks
//	codex   https://learn.chatgpt.com/docs/hooks, and .../docs/config-file/config-advanced for notify
//	grok    https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md
//
// A session-end event syncs. Each tool's per-turn hook event (Stop, stop)
// fires after every reply and runs while the person waits, so syncing there
// would put a fetch and a push after every answer; a sync publishes this
// machine's whole file, so a turn's record goes out with the next sync.
//
// Codex's hooks never sync: its SessionEnd hooks may run for at most 3 seconds,
// too short for a fetch and a push, and a sync killed part-way leaves its lock
// behind for ten minutes. Its notify program is different: Codex starts it and
// neither waits for it nor limits it, so a turn it records is synced at once, and
// a machine that runs only Codex still publishes.
var hookTools = map[string]hookTool{
	"claude": {cwd: stringField("cwd"), syncAtSessionEnd: true},
	// Cursor's common input has no cwd; workspace_roots is its working directory:
	// "normally just one" root.
	"cursor": {cwd: firstWorkspaceRoot, syncAtSessionEnd: true},
	// Codex's hooks send the event on stdin; its notify program receives it as
	// the last argument. Both carry cwd.
	"codex": {cwd: stringField("cwd"), argv: true},
	"grok":  {cwd: stringField("cwd"), syncAtSessionEnd: true},
}

// hookResult is what a hook did. It is printed only with --json, for a person
// trying a hook by hand; a tool's hook configuration never passes --json.
type hookResult struct {
	Tool     string          `json:"tool"`
	Event    string          `json:"event,omitempty"`
	Session  string          `json:"session,omitempty"`
	Recorded bool            `json:"recorded"`
	ID       string          `json:"id,omitempty"`
	Host     string          `json:"host,omitempty"`
	File     string          `json:"file,omitempty"`
	Type     string          `json:"type,omitempty"`
	Repo     string          `json:"repo,omitempty"`
	Branch   string          `json:"branch,omitempty"`
	Skipped  string          `json:"skipped,omitempty"`
	Synced   bool            `json:"synced"`
	Sync     *gitsync.Result `json:"sync,omitempty"`
	Problems []string        `json:"problems,omitempty"`
	Log      string          `json:"log,omitempty"`

	asJSON bool
}

func (r *hookResult) problem(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

// cmdHook always returns nil, so the process exits 0: see the comment at the top
// of this file.
func cmdHook(args []string, stdout, stderr io.Writer) (err error) {
	defer func() {
		// The last guard. An unrecovered panic exits 2, which is precisely the
		// code that keeps a Claude Code or Grok session from stopping.
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "fleetd hook: internal error: %v\n", r)
		}
		err = nil
	}()
	res := &hookResult{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				res.problem("internal error, recovered so the tool was not disturbed: %v", r)
			}
		}()
		runHook(res, args, stderr)
	}()
	if len(res.Problems) > 0 {
		if err := appendHookLog(res, time.Now()); err != nil {
			// Only stderr is left. None of these tools documents stderr changing
			// anything after exit 0.
			fmt.Fprintf(stderr, "fleetd hook: %s (and the log could not be written: %v)\n",
				strings.Join(res.Problems, "; "), err)
		}
	}
	if res.asJSON {
		_ = writeJSON(stdout, res)
	}
	return nil
}

func runHook(res *hookResult, args []string, stderr io.Writer) {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	// The flag package's own messages are not wanted on stderr: a bad flag is a
	// problem like any other, and goes to the log.
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print what the hook did; for running it by hand, never in a tool's configuration")
	dir := fs.String("dir", "", "journal directory, absolute (else $COMMS_CHANNELS/journal, else ~/.ai/channels/journal)")
	salt := fs.String("salt", "", "fleet salt (else the journal's fleetd.json, else $FLEET_SALT)")
	timeout := fs.Duration("timeout", hookSyncTimeout, "give up syncing after this long")
	noSync := fs.Bool("no-sync", false, "record only, even when a session ends")

	var rest []string
	if len(args) > 0 {
		res.Tool, rest = args[0], args[1:]
	}
	// Help goes to stderr and still exits 0. main exits 2 for help, and 2 is the
	// code that blocks a Stop.
	if res.Tool == "-h" || res.Tool == "--help" || res.Tool == "help" {
		fmt.Fprint(stderr, usage)
		return
	}
	parseErr := fs.Parse(rest)
	if errors.Is(parseErr, flag.ErrHelp) {
		fmt.Fprint(stderr, usage)
		return
	}
	res.asJSON = *asJSON

	// The log goes beside the journal directory, so that is placed first: every
	// problem after this point can then be written down.
	journalDir, dirErr := hookJournalDir(*dir)
	if dirErr == nil {
		res.Log = filepath.Join(filepath.Dir(journalDir), hookLogName)
	}
	if parseErr != nil {
		res.problem("%v", parseErr)
		return
	}
	tool, ok := hookTools[res.Tool]
	if !ok {
		res.problem("unknown tool %q: fleetd hook takes claude, cursor, codex or grok", res.Tool)
		return
	}
	if dirErr != nil {
		res.problem("%v", dirErr)
		return
	}
	if *timeout <= 0 {
		res.problem("--timeout must be positive, got %s", *timeout)
		return
	}

	raw, fromArgv, err := hookPayload(res, tool, fs.Args())
	if err != nil {
		res.problem("%v", err)
		return
	}
	p, err := parsePayload(raw)
	if err != nil {
		res.problem("%v", err)
		return
	}

	res.Event, res.Session = p.eventName(), p.sessionID()
	if why := skipReason(res.Tool, p); why != "" {
		res.Skipped = why
		return
	}
	kind := eventKind(res.Event)

	// A turn is recorded at most once per hookTurnGap per session, and that is
	// checked before git is asked anything, so the hook a tool runs after every
	// reply costs next to nothing when it records nothing. A turn dated in the
	// future, by a clock that ran fast, does not count.
	var warn bytes.Buffer
	if kind == "turn" && res.Session != "" {
		if fleetSalt, err := resolveSalt(*salt, journalDir, &warn); err == nil {
			h := identity(fleetSalt)
			file := filepath.Join(journalDir, journal.FileName(h.ID)+".jsonl")
			if at, ok := lastTurn(file, res.Tool, res.Session); ok && time.Since(at) >= 0 && time.Since(at) < hookTurnGap {
				res.Skipped = fmt.Sprintf("this session's turn was recorded at %s, less than %s ago", at.UTC().Format(time.RFC3339), hookTurnGap)
				// Codex's notify program is what publishes a Codex machine's
				// records, and the turn may have been recorded by Codex's own
				// Stop hook, which never syncs: so notify still syncs whatever
				// this machine has not published.
				if fromArgv && !*noSync && unsynced(journalDir, file) {
					syncHook(res, journalDir, h, *timeout)
				}
				return
			}
		}
		warn.Reset()
	}

	gitWait := hookGitWait
	if res.Tool == "codex" && !fromArgv {
		gitWait = codexGitWait
	}
	repo, branch, gitErr := lookupRepo(tool.cwd(p), gitWait)
	if gitErr != nil {
		res.problem("%v", gitErr)
	}
	res.Repo, res.Branch = repo, branch

	rec, err := appendRecord(recordRequest{
		dir:    journalDir,
		salt:   *salt,
		typ:    kind,
		note:   strings.TrimSpace(res.Tool + " " + res.Event),
		repo:   repo,
		branch: branch,
		agent:  res.Tool,
		extra:  map[string]string{"tool": res.Tool, "event": res.Event, "session": res.Session},
	}, &warn)
	if w := strings.TrimSpace(warn.String()); w != "" {
		w = strings.TrimPrefix(w, "fleetd: warning: ")
		// Without the journal's fleetd.json, init is what gives this machine the
		// fleet's salt; with it, the warning says what to do.
		if _, ok, err := gitsync.ReadFleet(journalDir); !ok && err == nil {
			w += ": run `fleetd init <journal repository URL>` on this machine, or its hook records may be filed apart from its others"
		}
		res.problem("%s", w)
	}
	if err != nil {
		res.problem("recording: %v", err)
		return
	}
	res.Recorded, res.ID, res.Host, res.File, res.Type = true, rec.id, rec.host.ID, rec.file, kind

	if *noSync || !(kind == "session" && tool.syncAtSessionEnd || fromArgv) {
		return
	}
	syncHook(res, journalDir, rec.host, *timeout)
}

// syncHook syncs the journal for a hook and notes the outcome in res.
func syncHook(res *hookResult, journalDir string, h hostOut, timeout time.Duration) {
	var noted bytes.Buffer
	synced, _, err := syncJournal(journalDir, h, timeout, false, &noted)
	if w := strings.TrimSpace(noted.String()); w != "" {
		res.problem("%s", strings.TrimPrefix(w, "fleetd: warning: "))
	}
	switch {
	case errors.Is(err, gitsync.ErrBusy):
		// Another hook's sync of this journal is running, and the next sync
		// publishes this machine's records; nothing is wrong.
		res.Skipped = "another sync of this journal was running; the next sync publishes this record"
	case err != nil:
		res.problem("sync: %v", err)
	default:
		res.Synced, res.Sync = true, &synced
	}
}

// unsynced reports whether this machine's journal file changed after its last
// successful sync began, or was never synced. A record written while that sync
// ran may have been written after it read the file.
func unsynced(journalDir, file string) bool {
	info, err := os.Stat(file)
	if err != nil {
		return false
	}
	st, ok := readSyncStatus(journalDir)
	if !ok || st.LastSuccess == nil {
		return true
	}
	since, err := time.Parse(time.RFC3339Nano, st.LastSuccess.Started)
	if st.LastSuccess.Started == "" {
		since, err = time.Parse(time.RFC3339, st.LastSuccess.At)
	}
	return err != nil || info.ModTime().After(since)
}

// lastTurn returns when this machine last recorded a turn of the given tool's
// session, reading only the end of its journal file: records are appended in
// time order, so the session's last turn, if it was recent, is near the end.
func lastTurn(file, tool, session string) (time.Time, bool) {
	f, err := os.Open(file)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	offset := info.Size() - hookTailBytes
	if offset < 0 {
		offset = 0
	}
	tail := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(tail, offset); err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, false
	}
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Type string         `json:"type"`
			TS   string         `json:"ts"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &rec) != nil || rec.Type != "turn" {
			continue
		}
		if rec.Data["tool"] == tool && rec.Data["session"] == session {
			at := parseTS(rec.TS)
			return at, !at.IsZero()
		}
	}
	return time.Time{}, false
}

// hookJournalDir resolves a hook's journal directory as the other commands do,
// but refuses a relative one: a hook runs in whatever directory the tool chose
// (the project, for Claude Code and Codex; ~/.cursor for Cursor's user hooks), so
// a relative journal would scatter records across every project worked in.
func hookJournalDir(flagValue string) (string, error) {
	dir, err := resolveDir(flagValue)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("journal directory %q is relative, and a hook runs in whatever directory the tool chose; "+
			"give --dir or COMMS_CHANNELS as an absolute path", dir)
	}
	return filepath.Clean(dir), nil
}

// hookPayload returns the event, and whether it came as an argument: the last
// argument, when the tool documents passing it that way and the argument is a
// JSON object, and otherwise stdin. Any other argument is reported and ignored,
// so a configuration that also passes, say, the event's name still records.
func hookPayload(res *hookResult, tool hookTool, rest []string) ([]byte, bool, error) {
	if n := len(rest); tool.argv && n > 0 && strings.HasPrefix(strings.TrimSpace(rest[n-1]), "{") {
		if n > 1 {
			res.problem("ignored unexpected arguments %q", rest[:n-1])
		}
		return []byte(rest[n-1]), true, nil
	}
	if len(rest) > 0 {
		res.problem("ignored unexpected arguments %q, and read the event from stdin", rest)
	}
	b, err := readStdin(os.Stdin)
	return b, false, err
}

// readStdin reads the event a tool pipes to its hook, bounded in size and in
// time. A terminal is not waited on: that is a person running the hook by hand,
// who would otherwise see it hang.
func readStdin(f *os.File) ([]byte, error) {
	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil, errors.New("no event on stdin: a tool pipes its event to the hook; to try one by hand, redirect a JSON file to stdin")
	}
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(f, hookPayloadMax+1))
		if err == nil && len(b) > hookPayloadMax {
			// Drain the rest, so the tool writing it is not left blocked on a full pipe.
			_, _ = io.Copy(io.Discard, f)
			err = fmt.Errorf("the event is larger than %d bytes", hookPayloadMax)
		}
		done <- read{b, err}
	}()
	select {
	case r := <-done:
		return r.b, r.err
	case <-time.After(hookStdinWait):
		return nil, fmt.Errorf("stdin was not closed within %s", hookStdinWait)
	}
}

// payload is a tool's event. It is kept as raw JSON by key so that fields fleetd
// does not use, and fields a newer tool adds, are ignored rather than refused.
type payload map[string]json.RawMessage

// parsePayload decodes an event. The tools write UTF-8, but on Windows a hook
// command usually runs under PowerShell, which re-encodes text it pipes to a
// program and can put a byte order mark in front, UTF-8 or UTF-16. JSON never
// begins with one, so a leading mark is unambiguous and is undone here rather
// than costing the record.
func parsePayload(raw []byte) (payload, error) {
	text := raw
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		text = raw[3:]
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		text = fromUTF16(raw[2:], binary.LittleEndian)
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		text = fromUTF16(raw[2:], binary.BigEndian)
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return nil, errors.New("the tool sent no event: stdin was empty")
	}
	var p payload
	if err := json.Unmarshal(text, &p); err != nil {
		return nil, fmt.Errorf("the event is not a JSON object (%d bytes): %v", len(raw), err)
	}
	if p == nil {
		return nil, errors.New("the event is JSON null, not an object")
	}
	return p, nil
}

func fromUTF16(b []byte, order binary.ByteOrder) []byte {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	return []byte(string(utf16.Decode(units)))
}

func (p payload) has(key string) bool {
	_, ok := p[key]
	return ok
}

// str returns a string field, or "" when it is absent or not a string.
func (p payload) str(key string) string {
	var s string
	raw, ok := p[key]
	if !ok || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// eventName is the event as the tool named it: hook_event_name from Claude Code,
// Cursor, Codex's hooks and Grok (which also sends camelCase hookEventName), and
// type from Codex's notify program.
func (p payload) eventName() string {
	for _, key := range []string{"hook_event_name", "hookEventName", "type"} {
		if v := p.str(key); v != "" {
			return v
		}
	}
	return ""
}

// sessionID is the tool's id for the session the event belongs to: session_id
// from Claude Code, Codex's hooks, Cursor's session events and Grok (which also
// sends sessionId), conversation_id from Cursor's other events, and thread-id
// from Codex's notify program.
func (p payload) sessionID() string {
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "thread-id"} {
		if v := p.str(key); v != "" {
			return v
		}
	}
	return ""
}

func stringField(key string) func(payload) string {
	return func(p payload) string { return p.str(key) }
}

func firstWorkspaceRoot(p payload) string {
	var roots []string
	if json.Unmarshal(p["workspace_roots"], &roots) != nil {
		return ""
	}
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return localPath(r, runtime.GOOS)
		}
	}
	return ""
}

// localPath undoes the URI-style path Cursor on Windows gives a workspace root,
// "/C:/Users/...", which Windows reads as a directory named "C:" on the current
// drive. Anywhere else, and in any other form, the path is returned as it is.
func localPath(p, goos string) string {
	if goos == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' &&
		(('a' <= p[1] && p[1] <= 'z') || ('A' <= p[1] && p[1] <= 'Z')) {
		return p[1:]
	}
	return p
}

// eventKey folds the tools' spellings of one event together: Claude Code's
// SessionEnd, Cursor's sessionEnd and Grok's session_end are all "sessionend".
func eventKey(event string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(event))
}

// eventKind is the record type for an event: "session" when a session ended,
// "turn" when a turn did, and "hook" for any other event a hook was attached to.
func eventKind(event string) string {
	switch eventKey(event) {
	case "sessionend":
		return "session"
	case "stop", "stopfailure", "stopcancelled", "agentturncomplete":
		return "turn"
	}
	return "hook"
}

// hostTool names the tool that actually ran this hook, when the event or the
// environment says so, and "" when neither does.
func hostTool(p payload) string {
	// Grok's events carry camelCase hookEventName, which no other tool sends
	// (Grok also sends hook_event_name, so that key proves nothing), and Grok sets
	// GROK_HOOK_EVENT for every hook it runs.
	if p.has("hookEventName") || os.Getenv("GROK_HOOK_EVENT") != "" {
		return "grok"
	}
	// Cursor sets CURSOR_VERSION for every hook it runs. Its own events also carry
	// cursor_version, but whether the events it hands Claude Code's hooks do is
	// not documented, so the environment is what identifies it there.
	if os.Getenv("CURSOR_VERSION") != "" || p.has("cursor_version") {
		return "cursor"
	}
	return ""
}

// skipReason says why an event is not recorded, or "" when it is.
func skipReason(tool string, p payload) string {
	// Grok runs Claude Code's and Cursor's user hooks, and Cursor can run Claude
	// Code's, so one configured hook also fires inside the other tools' sessions.
	// Recording such an event under the configured tool's name would misattribute
	// it, and under the host's name would duplicate what the host's own hook
	// records.
	if host := hostTool(p); host != "" && host != tool {
		return fmt.Sprintf("%s ran %s's hook; `fleetd hook %s` in %s's own hook configuration records %s's sessions",
			host, tool, host, host, host)
	}
	if tool == "grok" {
		// "Exit early when subagentType is present. A subagent's stop is not the
		// session's."
		if p.str("subagentType") != "" {
			return "a subagent's event; the session's own events are recorded"
		}
		// Grok fires one more Stop when the session ends, with reason
		// channel_closed or shutdown; a genuine turn end has reason end_turn. The
		// session's SessionEnd records the session.
		if reason := p.str("reason"); eventKey(p.eventName()) == "stop" && reason != "" && reason != "end_turn" {
			return fmt.Sprintf("Grok's session-end Stop (reason %q); its SessionEnd records the session", reason)
		}
	}
	return ""
}

// lookupRepo is repoAndBranch, as a variable so a test can make it fail in ways
// a real repository cannot.
var lookupRepo = repoAndBranch

// repoAndBranch names the repository and branch at cwd: the basename of
// `git -C <cwd> rev-parse --show-toplevel` and `git -C <cwd> branch
// --show-current`. With no cwd, or outside any repository, both are empty and the
// event is still recorded, since which machine did something is worth knowing
// without them. A failure that silently drops them from every record, such as git
// refusing a repository it does not trust, is returned so it gets logged.
func repoAndBranch(cwd string, wait time.Duration) (repo, branch string, err error) {
	if strings.TrimSpace(cwd) == "" {
		return "", "", nil
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("the event's working directory %s is not a directory on this machine, so repo and branch are not recorded", cwd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	out, err := gitsync.Git(ctx, "", nil, "-C", cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		switch {
		case errors.Is(err, osexec.ErrNotFound):
			return "", "", errors.New("git is not on PATH, so repo and branch are not recorded")
		case ctx.Err() != nil:
			return "", "", fmt.Errorf("git did not name the repository at %s within %s", cwd, wait)
		case !underGitCheckout(cwd):
			// Not a repository: nothing to record, and nothing wrong.
			return "", "", nil
		}
		return "", "", fmt.Errorf("git could not read the repository at %s, so repo and branch are not recorded: %v", cwd, err)
	}
	top := strings.TrimSpace(out)
	// A repository whose root is the home directory is named after the account,
	// and these records are committed; the account name is published only with
	// `fleetd record --include-user`.
	if home, err := os.UserHomeDir(); err == nil && gitsync.SameDir(top, home) {
		return "", "", nil
	}
	repo = filepath.Base(top)
	if repo == "." || repo == "/" || repo == `\` {
		repo = ""
	}
	// Empty on a detached HEAD, which is then recorded without a branch.
	out, err = gitsync.Git(ctx, "", nil, "-C", cwd, "branch", "--show-current")
	if err != nil {
		if ctx.Err() != nil {
			return repo, "", fmt.Errorf("git did not name the branch at %s within %s", cwd, wait)
		}
		return repo, "", fmt.Errorf("git could not name the branch at %s: %v", cwd, err)
	}
	return repo, strings.TrimSpace(out), nil
}

// underGitCheckout reports whether dir, or a directory above it, holds .git. It
// is what tells git failing on a repository from a directory outside any.
func underGitCheckout(dir string) bool {
	for d := filepath.Clean(dir); ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// appendHookLog appends res's problems to its log, one line each, first moving a
// log that has reached hookLogMax aside so the log never grows without bound.
func appendHookLog(res *hookResult, now time.Time) error {
	if res.Log == "" {
		return errors.New("there is no journal directory to put " + hookLogName + " beside")
	}
	if fi, err := os.Lstat(res.Log); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through the symbolic link %s", res.Log)
		}
		if fi.Size() >= hookLogMax {
			// Another hook may have moved it first.
			if err := os.Rename(res.Log, res.Log+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(res.Log), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, p := range res.Problems {
		fmt.Fprintf(&b, "%s %s %s: %s\n", now.UTC().Format(time.RFC3339),
			oneLine(orDash(res.Tool)), oneLine(orDash(res.Event)), oneLine(p))
	}
	f, err := os.OpenFile(res.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// oneLine keeps each problem on one log line; git's messages span several.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
