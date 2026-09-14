package main

import (
	"bytes"
	"encoding/json"
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

func TestPluralReadsLikeEnglish(t *testing.T) {
	for n, want := range map[int]string{0: "0 records", 1: "1 record", 2: "2 records"} {
		if got := plural(n, "record"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}
