package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// exec drives the command the way a shell does, and returns what a user would
// see. run() takes its writers as arguments precisely so this is possible without
// a subprocess or a captured global.
func exec(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	err = run(args, &o, &e)
	return o.String(), e.String(), err
}

func TestNoArgsPrintsUsageAndFails(t *testing.T) {
	_, stderr, err := exec(t)
	if err == nil {
		t.Error("invoking with no command succeeded; it should explain itself and fail")
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("stderr did not contain usage text: %q", stderr)
	}
}

func TestUnknownCommandNamesItAndShowsUsage(t *testing.T) {
	_, stderr, err := exec(t, "wat")
	if err == nil || !strings.Contains(err.Error(), `"wat"`) {
		t.Errorf("err = %v, want it to quote the unknown command", err)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Error("an unknown command should also show usage")
	}
}

func TestHostJSONCarriesTheHonestAttributionFields(t *testing.T) {
	stdout, _, err := exec(t, "host", "--json", "--salt", "t")
	if err != nil {
		t.Fatal(err)
	}
	var h hostOut
	if err := json.Unmarshal([]byte(stdout), &h); err != nil {
		t.Fatalf("host --json did not emit valid JSON: %v\n%s", err, stdout)
	}
	if h.ID == "" || h.Name == "" || h.OS == "" || h.Source == "" {
		t.Errorf("required identity fields are empty: %+v", h)
	}
	if !strings.HasPrefix(h.ID, "host:") {
		t.Errorf("ID = %q, want a host: prefixed digest", h.ID)
	}
	// The digest must not be the hostname in disguise.
	if strings.Contains(strings.ToLower(h.ID), strings.ToLower(h.Name)) {
		t.Errorf("ID %q appears to embed the hostname %q", h.ID, h.Name)
	}
}

func TestMissingSaltWarnsRatherThanSilentlyWeakening(t *testing.T) {
	t.Setenv("FLEET_SALT", "")
	_, stderr, err := exec(t, "host")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "no --salt") {
		t.Errorf("stderr = %q, want a warning that host digests are unseparated", stderr)
	}
}

func TestSaltFromEnvironmentIsUsedAndSeparatesIdentity(t *testing.T) {
	read := func(salt string) string {
		t.Setenv("FLEET_SALT", salt)
		stdout, _, err := exec(t, "host", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var h hostOut
		if err := json.Unmarshal([]byte(stdout), &h); err != nil {
			t.Fatal(err)
		}
		return h.ID
	}
	if read("fleet-a") == read("fleet-b") {
		t.Error("FLEET_SALT had no effect on the derived identity")
	}
}

func TestRecordRequiresAType(t *testing.T) {
	_, _, err := exec(t, "record", "--dir", t.TempDir(), "--salt", "t", "--note", "x")
	if err == nil || !strings.Contains(err.Error(), "--type") {
		t.Errorf("err = %v, want a complaint that --type is required", err)
	}
}

func TestRecordRejectsABadTimestamp(t *testing.T) {
	_, _, err := exec(t, "record", "--dir", t.TempDir(), "--salt", "t",
		"--type", "note", "--at", "last tuesday")
	if err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("err = %v, want an RFC3339 complaint rather than a silently wrong timestamp", err)
	}
}

func TestWhereOnAnEmptyStoreSaysSoAndSucceeds(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := exec(t, "where", "--dir", dir)
	if err != nil {
		t.Fatalf("an empty journal is not an error: %v", err)
	}
	if !strings.Contains(stdout, "no records") {
		t.Errorf("stdout = %q, want it to say the store is empty", stdout)
	}
}

func TestRecordThenWhereAnswersTheQuestion(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "handoff",
		"--repo", "ai-workspace", "--branch", "main", "--agent", "claude/cloud",
		"--at", "2026-09-14T13:20:00Z", "--note", "merged PR 2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "observation",
		"--repo", "agent-comms", "--at", "2026-09-14T13:55:00Z", "--note", "journal green"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []whereEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("where --json did not emit valid JSON: %v\n%s", err, stdout)
	}
	if len(got) != 1 {
		t.Fatalf("got %d machines, want 1 — both records came from this host", len(got))
	}
	e := got[0]
	if e.Records != 2 {
		t.Errorf("Records = %d, want 2", e.Records)
	}
	// "Where did I leave off" means the LAST thing, not the first.
	if e.LastRepo != "agent-comms" || e.LastNote != "journal green" {
		t.Errorf("last entry = %s / %q, want agent-comms / journal green", e.LastRepo, e.LastNote)
	}
	if e.LastTS != "2026-09-14T13:55:00Z" {
		t.Errorf("LastTS = %q, want the later timestamp", e.LastTS)
	}
}

func TestWhereSeparatesMachinesAndOrdersNewestFirst(t *testing.T) {
	dir := t.TempDir()
	// Two salts give two distinct host identities, which is how a second machine
	// appears without needing a second machine.
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "machine-a", "--type", "note",
		"--at", "2026-09-14T09:00:00Z", "--note", "older, on A"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "machine-b", "--type", "note",
		"--at", "2026-09-14T17:00:00Z", "--note", "newer, on B"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []whereEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2", len(got))
	}
	if got[0].LastNote != "newer, on B" {
		t.Errorf("first entry is %q, want the most recent machine first", got[0].LastNote)
	}
	if got[0].Host == got[1].Host {
		t.Error("the two machines were folded into one host")
	}
}

func TestRecordedLineCarriesHostAttribution(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "note",
		"--at", "2026-09-14T13:00:00Z", "--note", "n", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		ID   string `json:"id"`
		Host string `json:"host"`
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("record --json did not emit valid JSON: %v\n%s", err, stdout)
	}
	if !strings.HasPrefix(res.ID, "hive:") {
		t.Errorf("id = %q, want a content-derived hive: id", res.ID)
	}
	if res.Host == "" || res.File == "" {
		t.Errorf("record did not report where it wrote: %+v", res)
	}

	// The written record must be readable back with its host facts intact, or the
	// attribution this whole tool exists to provide is not actually stored.
	where, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, `"host_name"`) {
		t.Errorf("where output carries no host_name: %s", where)
	}
}

func TestMachinesAreOrderedByInstantNotByStringCompare(t *testing.T) {
	// The wrong answer this pins: lexical comparison of RFC3339 is only correct
	// when every timestamp is UTC "Z". Machine A at 10:00+05:00 is 05:00Z, which
	// is EARLIER than machine B at 06:00Z, but sorts later as a string. That
	// matters beyond --at: hive/cell.py writes every ts as
	// datetime.now(UTC).astimezone().isoformat(), i.e. local time with an offset,
	// so records from the existing Python bus sorted wrong by construction.
	dir := t.TempDir()
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "machine-a", "--type", "note",
		"--at", "2026-09-14T10:00:00+05:00", "--note", "actually older"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "machine-b", "--type", "note",
		"--at", "2026-09-14T06:00:00Z", "--note", "actually newer"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []whereEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2", len(got))
	}
	if got[0].LastNote != "actually newer" {
		t.Errorf("first entry is %q; 06:00Z is later than 10:00+05:00 (=05:00Z), so ordering is still lexical",
			got[0].LastNote)
	}
}

func TestTwoRecordsInTheSameSecondAreDistinct(t *testing.T) {
	// At whole-second resolution two successive records produced byte-identical
	// cells with the same content-derived id. Since a reader is documented to
	// collapse colliding ids, one of two genuinely distinct events would simply
	// disappear while `where` still counted both.
	dir := t.TempDir()
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		stdout, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "observation",
			"--note", "same event twice in a second", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatal(err)
		}
		ids[res.ID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("two records produced %d distinct id(s); a distinct event was silently merged away", len(ids))
	}
}

func TestOSAccountIsNotPublishedUnlessAskedFor(t *testing.T) {
	// internal/hostid deliberately publishes only a digest of the machine
	// identifier. Emitting the OS account in the same record would undo that: on
	// a domain-joined Windows host user.Current().Username is DOMAIN\account, so
	// the default behaviour committed the AD domain and the operator's account
	// name into a repository on every record.
	dir := t.TempDir()
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "note",
		"--at", "2026-09-14T12:00:00Z", "--note", "n"); err != nil {
		t.Fatal(err)
	}
	raw := readJournal(t, dir)
	if strings.Contains(raw, `"user"`) {
		t.Errorf("the OS account was published without --include-user: %s", raw)
	}

	dir2 := t.TempDir()
	if _, _, err := exec(t, "record", "--dir", dir2, "--salt", "t", "--type", "note",
		"--at", "2026-09-14T12:00:00Z", "--note", "n", "--include-user"); err != nil {
		t.Fatal(err)
	}
	if raw2 := readJournal(t, dir2); !strings.Contains(raw2, `"user"`) {
		t.Errorf("--include-user did not publish the account: %s", raw2)
	}
}

func TestLimitAppliesToJSONAsWellAsTheTerminal(t *testing.T) {
	// --limit used to be read only by the human branch, so a program asking for
	// the last N entries silently received one and could not tell.
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "note",
			"--at", "2026-09-14T12:00:0"+strconv.Itoa(i)+"Z", "--note", "n"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	stdout, _, err := exec(t, "where", "--dir", dir, "--json", "--limit", "3")
	if err != nil {
		t.Fatal(err)
	}
	var got []whereEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d machines, want 1", len(got))
	}
	// --limit counts total records shown per machine, so 3 means the last one
	// (reported in last_*) plus the 2 before it in recent.
	if len(got[0].Recent) != 2 {
		t.Fatalf("recent has %d entries, want 2 — --limit was ignored in the JSON branch", len(got[0].Recent))
	}
	// Newest first, and excluding the one already reported as last_*.
	if got[0].Recent[0].Note != "n3" {
		t.Errorf("first recent entry is %q, want n3", got[0].Recent[0].Note)
	}
}

func TestOutOfOrderTimestampsAreFlaggedNotHidden(t *testing.T) {
	// Append order is what gets reported, because it is what actually happened
	// and does not depend on a clock. When a host's own timestamps disagree with
	// that order, presenting the file-order-last entry as "last" is still correct
	// but reads wrong, so it is called out rather than left to confuse.
	dir := t.TempDir()
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "handoff",
		"--at", "2026-09-22T06:00:00Z", "--note", "written first, later timestamp"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "observation",
		"--at", "2026-09-22T05:00:00Z", "--note", "written second, earlier timestamp"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []whereEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].TimestampsOutOfOrder {
		t.Fatalf("out-of-order timestamps were not flagged: %+v", got)
	}
	// Append order still decides which record is "last".
	if got[0].LastNote != "written second, earlier timestamp" {
		t.Errorf("last is %q; append order must decide, not the clock", got[0].LastNote)
	}

	human, _, err := exec(t, "where", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human, "clock or write order is suspect") {
		t.Errorf("terminal output does not mention the disagreement:\n%s", human)
	}
}

func TestMonotonicTimestampsAreNotFlagged(t *testing.T) {
	dir := t.TempDir()
	for i, ts := range []string{"2026-09-22T05:00:00Z", "2026-09-22T06:00:00Z"} {
		if _, _, err := exec(t, "record", "--dir", dir, "--salt", "t", "--type", "note",
			"--at", ts, "--note", "n"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	stdout, _, err := exec(t, "where", "--dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []whereEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got[0].TimestampsOutOfOrder {
		t.Error("in-order timestamps were flagged as out of order")
	}
}

func TestLimitBelowOneIsRejected(t *testing.T) {
	if _, _, err := exec(t, "where", "--dir", t.TempDir(), "--limit", "0"); err == nil {
		t.Error("--limit 0 was accepted")
	}
}

func TestMistypedDirIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "channles", "journal")
	stdout, _, err := exec(t, "where", "--dir", missing)
	if err == nil {
		t.Fatalf("a nonexistent store answered successfully: %q", stdout)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v, want it to say the store does not exist", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("the read created %s; only record may create the store", missing)
	}
}

// readJournal returns the single journal file's contents from a store.
func readJournal(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatal(rerr)
			}
			return string(b)
		}
	}
	t.Fatalf("no journal file in %s", dir)
	return ""
}

func TestPluralReadsLikeEnglish(t *testing.T) {
	for n, want := range map[int]string{0: "0 records", 1: "1 record", 2: "2 records"} {
		if got := plural(n, "record"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestVersionNamesTheBuildAndPlatform(t *testing.T) {
	stdout, _, err := exec(t, "version")
	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	fields := strings.Fields(stdout)
	if len(fields) != 3 || fields[0] != "fleetd" || !strings.HasPrefix(fields[1], "dev") || !strings.Contains(fields[2], "/") {
		t.Errorf("version printed %q, want \"fleetd dev[-<commit>] <os>/<arch>\"", stdout)
	}
}

func TestAReleaseBuildReportsItsTag(t *testing.T) {
	old := version
	version = "fleetd-v9.8.7"
	defer func() { version = old }()
	stdout, _, err := exec(t, "--version")
	if err != nil || !strings.HasPrefix(stdout, "fleetd fleetd-v9.8.7 ") {
		t.Errorf("--version printed %q (err %v), want the tag the release build set", stdout, err)
	}
}
