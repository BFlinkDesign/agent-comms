package cell

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// repoRoot locates the agent-comms checkout from this source file's own path,
// so the test works from any working directory and in any clone location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

// pythonID asks the REAL hive.cell module for the ID it would derive. This is the
// whole point of the test: it compares against the running Python implementation,
// not against a hardcoded expectation copied from it. A hardcoded vector would
// keep passing after hive/cell.py changed; this will not.
func pythonID(t *testing.T, typ, from, ts, channel, dataJSON string) string {
	t.Helper()
	const script = `
import json, sys
from hive.cell import _generate_id
typ, frm, ts, channel, data_json = sys.argv[1:6]
sys.stdout.write(_generate_id(typ, frm, ts, channel, json.loads(data_json)))
`
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not installed, cannot compare against the reference implementation: %v", err)
	}
	cmd := exec.Command("python3", "-c", script, typ, from, ts, channel, dataJSON)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Deliberately a failure, not a skip. python3 exists, so a non-zero exit
		// means either hive.cell is broken or this test's own fixture is invalid.
		// Skipping here would silently turn a malformed fixture into a green run,
		// which is the "gate that cannot fail" failure mode.
		t.Fatalf("reference implementation rejected the case (this is a real failure, not an unavailable tool): %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// canonicalFromPython renders data the way json.dumps(separators=(",",":"),
// sort_keys=True) does, so a divergence report shows the exact digest input
// both sides used rather than only the differing hashes.
func canonicalFromPython(t *testing.T, dataJSON string) string {
	t.Helper()
	const script = `
import json, sys
sys.stdout.write(json.dumps(json.loads(sys.argv[1]), separators=(",",":"), sort_keys=True))
`
	out, err := exec.Command("python3", "-c", script, dataJSON).Output()
	if err != nil {
		return "<unavailable>"
	}
	return string(out)
}

func TestPythonConformance(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		from     string
		ts       string
		channel  string
		data     map[string]Value
		dataJSON string
	}{
		{
			name: "empty data", typ: "note", from: "claude/dev",
			ts: "2026-09-14T12:00:00+00:00", channel: "general",
			data: map[string]Value{}, dataJSON: `{}`,
		},
		{
			name: "flat ascii", typ: "task", from: "codex/worker",
			ts: "2026-09-14T12:00:00-05:00", channel: "general",
			data:     map[string]Value{"msg": S("hello there friend"), "n": I(42), "ok": B(true)},
			dataJSON: `{"msg":"hello there friend","n":42,"ok":true}`,
		},
		{
			// Key order must not affect the digest: json.dumps sorts keys, and so
			// must we, or two hosts recording the same fact would disagree.
			name: "key order is irrelevant", typ: "task", from: "a/b",
			ts: "2026-09-14T12:00:00Z", channel: "work",
			data:     map[string]Value{"zebra": I(1), "alpha": I(2), "Mango": I(3), "_under": I(4)},
			dataJSON: `{"zebra":1,"alpha":2,"Mango":3,"_under":4}`,
		},
		{
			// Real commit subjects contain non-ASCII. Python's default
			// ensure_ascii=True escapes these; if Go emitted them raw the IDs
			// would silently diverge exactly on real-world data.
			name: "non-ascii", typ: "trace", from: "gemini/cli",
			ts: "2026-09-14T12:00:00Z", channel: "general",
			data:     map[string]Value{"subject": S("café — naïve résumé · ±90°")},
			dataJSON: `{"subject":"café — naïve résumé · ±90°"}`,
		},
		{
			// Astral-plane runes must become a UTF-16 surrogate pair.
			name: "emoji astral plane", typ: "trace", from: "cursor/agent",
			ts: "2026-09-14T12:00:00Z", channel: "general",
			data:     map[string]Value{"subject": S("shipped \U0001F680 done ✅ \U0001D54F")},
			dataJSON: `{"subject":"shipped 🚀 done ✅ 𝕏"}`,
		},
		{
			name: "escapes and control chars", typ: "note", from: "a/b",
			ts: "2026-09-14T12:00:00Z", channel: "general",
			data: map[string]Value{"s": S("quote\" back\\ tab\t nl\n cr\r ctrl")},
			// The escape is concatenated rather than written literally: a raw
			// U+0001 byte in the fixture is not valid JSON, and an editor or
			// generator that unescapes the source would silently produce one.
			dataJSON: `{"s":"quote\" back\\ tab\t nl\n cr\r ctrl` + "\\u0001" + `"}`,
		},
		{
			name: "nested object and array", typ: "result", from: "a/b",
			ts: "2026-09-14T12:00:00Z", channel: "general",
			data: map[string]Value{
				"host":  O{"name": S("CNC-1"), "os": S("windows"), "cores": I(24)},
				"repos": A{S("ai-workspace"), S("agent-comms")},
				"empty": O{},
				"list":  A{},
			},
			dataJSON: `{"host":{"name":"CNC-1","os":"windows","cores":24},"repos":["ai-workspace","agent-comms"],"empty":{},"list":[]}`,
		},
		{
			name: "negative and zero", typ: "metric", from: "a/b",
			ts: "2026-09-14T12:00:00Z", channel: "general",
			data:     map[string]Value{"delta": I(-17), "zero": I(0), "off": B(false)},
			dataJSON: `{"delta":-17,"zero":0,"off":false}`,
		},
		{
			// U+FFFD. An earlier encoder special-cased this and emitted it raw,
			// which was the single divergence from Python in the entire encoder
			// and which this table did not cover. Ranging a Go string yields it
			// for every invalid UTF-8 byte too, so a note carrying a stray cp1252
			// byte took the same path.
			name: "replacement character", typ: "note", from: "a/b",
			ts: "2026-09-14T12:00:00Z", channel: "general",
			data:     map[string]Value{"s": S("bad�end")},
			dataJSON: `{"s":"bad�end"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := pythonID(t, tc.typ, tc.from, tc.ts, tc.channel, tc.dataJSON)
			got := DeriveID(tc.typ, tc.from, tc.ts, tc.channel, tc.data)
			if got != want {
				t.Errorf("ID mismatch\n  go:     %s\n  python: %s\n  go canonical:     %s\n  python canonical: %s",
					got, want, CanonicalData(tc.data), canonicalFromPython(t, tc.dataJSON))
			}
		})
	}
}

// TestEveryCodePointMatchesPython sweeps the Unicode space rather than trusting a
// hand-picked table. The eight-case table above is a readable summary of the
// encoder's decisions; it is not evidence, and it demonstrably missed U+FFFD. A
// single escaping decision that differs for one character silently breaks the
// only property this package promises, so the space is checked exhaustively
// across the ranges that matter and by sampling the rest.
func TestEveryCodePointMatchesPython(t *testing.T) {
	if testing.Short() {
		t.Skip("sweeps the Unicode space; runs in the full suite")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not installed: %v", err)
	}

	var points []rune
	// Exhaustive where escaping rules change: controls, ASCII, Latin-1, the
	// 0x7f boundary, and the edges of the basic multilingual plane.
	for r := rune(0); r <= 0x2FF; r++ {
		points = append(points, r)
	}
	for r := rune(0xFF00); r <= 0xFFFF; r++ {
		points = append(points, r)
	}
	// Sampled across the rest of the BMP and the astral planes, where the
	// surrogate-pair path runs.
	for r := rune(0x300); r < 0xFF00; r += 7 {
		points = append(points, r)
	}
	for r := rune(0x10000); r <= 0x10FFFF; r += 401 {
		points = append(points, r)
	}

	// Surrogate code points cannot appear in a Go string: the compiler and
	// utf8 both render them as U+FFFD, which is covered by the table above.
	filtered := points[:0]
	for _, r := range points {
		if r < 0xD800 || r > 0xDFFF {
			filtered = append(filtered, r)
		}
	}
	points = filtered

	// Batched: one python3 process per batch rather than per code point, which
	// keeps a sweep of this size to a few seconds.
	const batch = 256
	for start := 0; start < len(points); start += batch {
		end := min(start+batch, len(points))
		data := map[string]Value{}
		obj := map[string]string{}
		for i, r := range points[start:end] {
			key := "k" + strconv.Itoa(i)
			v := string(r)
			data[key] = S(v)
			obj[key] = v
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("encoding fixture for batch at %d: %v", start, err)
		}

		want := pythonID(t, "sweep", "a/b", "2026-09-14T12:00:00Z", "general", string(encoded))
		if got := DeriveID("sweep", "a/b", "2026-09-14T12:00:00Z", "general", data); got != want {
			// Narrow to the offending code point so the failure names it.
			for i, r := range points[start:end] {
				one := map[string]Value{"k": S(string(r))}
				enc, _ := json.Marshal(map[string]string{"k": string(r)})
				w := pythonID(t, "sweep", "a/b", "2026-09-14T12:00:00Z", "general", string(enc))
				if g := DeriveID("sweep", "a/b", "2026-09-14T12:00:00Z", "general", one); g != w {
					t.Fatalf("code point U+%04X (index %d) diverges: go %s (%s), python %s",
						r, start+i, g, CanonicalData(one), w)
				}
			}
			t.Fatalf("batch at %d diverges (%s vs %s) but no single code point did", start, got, want)
		}
	}
	t.Logf("swept %d code points with no divergence", len(points))
}

func TestNewDoesNotAliasTheCallersData(t *testing.T) {
	// The batching shape this guards against: one map reused across a loop. With
	// the map aliased, every cell ends up sharing the final payload while each
	// carries the id of the payload at its own construction, so no line's id
	// hashes to its own data and content addressing silently stops working.
	shared := map[string]Value{"n": I(1)}
	refs := []string{"hive:aaa"}
	tags := []string{"first"}

	c1, err := New("note", "a/b", "2026-09-14T12:00:00Z", "general", shared, refs, tags, 0)
	if err != nil {
		t.Fatal(err)
	}
	shared["n"] = I(2)
	refs[0] = "hive:bbb"
	tags[0] = "second"

	if got := DeriveID("note", "a/b", "2026-09-14T12:00:00Z", "general", map[string]Value{"n": I(1)}); c1.ID != got {
		t.Errorf("cell id changed meaning after the caller mutated its map: %s, want %s", c1.ID, got)
	}
	if !strings.Contains(c1.Marshal(), `"n":1`) {
		t.Errorf("marshalled cell reflects the caller's later mutation: %s", c1.Marshal())
	}
	if !strings.Contains(c1.Marshal(), "hive:aaa") || !strings.Contains(c1.Marshal(), "first") {
		t.Errorf("refs or tags reflect the caller's later mutation: %s", c1.Marshal())
	}
}

func TestDeterministicAcrossMapIteration(t *testing.T) {
	// Go randomizes map iteration order on purpose. Derive the same payload many
	// times; any reliance on iteration order shows up as a differing ID.
	data := map[string]Value{"a": I(1), "b": I(2), "c": I(3), "d": I(4), "e": I(5), "f": I(6), "g": I(7), "h": I(8)}
	first := DeriveID("t", "f", "2026-09-14T12:00:00Z", "c", data)
	for i := 0; i < 500; i++ {
		if got := DeriveID("t", "f", "2026-09-14T12:00:00Z", "c", data); got != first {
			t.Fatalf("iteration %d produced %s, want %s", i, got, first)
		}
	}
}

func TestSameFactSameIDRegardlessOfKeyOrder(t *testing.T) {
	// The property the whole multi-writer design rests on: two hosts recording
	// the identical observation produce the identical ID, so a duplicate is
	// detectable. This is precisely what uuid4 IDs cannot provide.
	data := map[string]Value{"repo": S("ai-workspace"), "sha": S("31158a76")}
	a := DeriveID("observation", "fleet/collector", "2026-09-14T12:00:00Z", "journal", data)
	b := DeriveID("observation", "fleet/collector", "2026-09-14T12:00:00Z", "journal",
		map[string]Value{"sha": S("31158a76"), "repo": S("ai-workspace")})
	if a != b {
		t.Fatalf("identical observations produced different IDs: %s vs %s", a, b)
	}
}

func TestRejectsEmptyRequiredFields(t *testing.T) {
	for _, bad := range []struct{ typ, from, ts, channel string }{
		{"", "a/b", "2026-09-14T12:00:00Z", "c"},
		{"t", "", "2026-09-14T12:00:00Z", "c"},
		{"t", "a/b", "", "c"},
		{"t", "a/b", "2026-09-14T12:00:00Z", ""},
		{"t", "a/b", "2026-09-14T12:00:00Z", "   "},
	} {
		if _, err := New(bad.typ, bad.from, bad.ts, bad.channel, nil, nil, nil, 0); err == nil {
			t.Errorf("New(%q,%q,%q,%q) accepted an empty required field", bad.typ, bad.from, bad.ts, bad.channel)
		}
	}
}

func TestMarshalIsOneLine(t *testing.T) {
	// A cell is one JSONL record. An embedded newline would corrupt every
	// subsequent record in the file, so the encoder must escape it.
	c, err := New("note", "a/b", "2026-09-14T12:00:00Z", "general",
		map[string]Value{"msg": S("line one\nline two")}, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	line := c.Marshal()
	if strings.Contains(line, "\n") {
		t.Fatalf("marshalled cell contains a raw newline: %q", line)
	}
	if !strings.Contains(line, `\n`) {
		t.Fatalf("newline was not escaped: %q", line)
	}
}

func TestMarshalIDMatchesDerivedID(t *testing.T) {
	// A record whose stored id disagrees with its own content is unverifiable,
	// which would defeat the entire point of content addressing.
	data := map[string]Value{"repo": S("agent-comms"), "n": I(3)}
	c, err := New("observation", "fleet/collector", "2026-09-14T12:00:00Z", "journal", data, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := DeriveID("observation", "fleet/collector", "2026-09-14T12:00:00Z", "journal", data); c.ID != want {
		t.Fatalf("cell carries id %s but its content derives %s", c.ID, want)
	}
	if !strings.Contains(c.Marshal(), c.ID) {
		t.Fatalf("marshalled line does not carry the cell id: %s", c.Marshal())
	}
}
