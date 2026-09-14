// Package cell implements the HIVE cell: the atomic, immutable, content-addressed
// unit of the bus.
//
// The ID derivation here is byte-for-byte compatible with hive/cell.py's
// _generate_id: "hive:" + SHA-256(type + from + ts + channel + JSON(data))[:16],
// where JSON(data) is Python's json.dumps(data, separators=(",",":"), sort_keys=True).
//
// Compatibility is not asserted, it is tested: TestPythonConformance in
// cell_test.go shells out to the real hive.cell module and requires identical
// IDs for a table of payloads. If the Python formula changes, that test fails.
//
// Note on the bus as it stands: hive/shell_write.py does NOT use this formula --
// it assigns uuid4 and emits a different field set. This package deliberately
// implements the documented content-addressed contract, because a random ID
// cannot support cross-machine duplicate detection: two hosts publishing the
// same observation must produce the same ID for dedup to be possible at all.
package cell

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Version is the cell schema version written into every cell.
const Version = 1

// Cell is an immutable HIVE cell. Construct with New; do not mutate after.
type Cell struct {
	ID      string
	V       int
	Type    string
	From    string
	TS      string
	Channel string
	Data    map[string]Value
	Refs    []string
	TTL     int
	Tags    []string
}

// Value is a JSON value restricted to the shapes a cell payload may carry.
// Restricting the type is what lets Marshal be total: there is no encoding
// path that can fail at runtime, so callers never have to handle a serializer
// error that cannot actually occur.
type Value interface{ writeCanonical(*strings.Builder) }

type (
	// S is a JSON string.
	S string
	// I is a JSON integer.
	I int64
	// B is a JSON boolean.
	B bool
	// A is a JSON array.
	A []Value
	// O is a JSON object.
	O map[string]Value
)

func (v S) writeCanonical(b *strings.Builder) { writeJSONString(b, string(v)) }
func (v I) writeCanonical(b *strings.Builder) { b.WriteString(strconv.FormatInt(int64(v), 10)) }

func (v B) writeCanonical(b *strings.Builder) {
	if v {
		b.WriteString("true")
		return
	}
	b.WriteString("false")
}

func (v A) writeCanonical(b *strings.Builder) {
	b.WriteByte('[')
	for i, e := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		e.writeCanonical(b)
	}
	b.WriteByte(']')
}

func (v O) writeCanonical(b *strings.Builder) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	// json.dumps(sort_keys=True) sorts by Unicode code point, which is exactly
	// what Go's sort.Strings does on UTF-8 bytes.
	sort.Strings(keys)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONString(b, k)
		b.WriteByte(':')
		v[k].writeCanonical(b)
	}
	b.WriteByte('}')
}

// writeJSONString emits a JSON string escaped the way Python's json.dumps does
// with its default ensure_ascii=True: every non-ASCII rune becomes a \uXXXX
// escape, and astral-plane runes become a surrogate pair. Matching this exactly
// is required for ID equality on any payload containing non-ASCII text -- which
// real commit subjects and file paths routinely do.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case r < 0x7f:
				b.WriteRune(r)
			case r == utf8.RuneError:
				// An invalid UTF-8 byte decodes to RuneError. Python would raise
				// on undecodable input rather than silently substitute, so
				// encode the replacement character explicitly and let the
				// conformance test surface any divergence.
				b.WriteString(`�`)
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
}

// CanonicalData renders data exactly as json.dumps(data, separators=(",",":"),
// sort_keys=True) would, which is the string the ID digest is taken over.
func CanonicalData(data map[string]Value) string {
	var b strings.Builder
	O(data).writeCanonical(&b)
	return b.String()
}

// DeriveID computes the content-addressed cell ID.
func DeriveID(typ, from, ts, channel string, data map[string]Value) string {
	sum := sha256.Sum256([]byte(typ + from + ts + channel + CanonicalData(data)))
	return "hive:" + hex.EncodeToString(sum[:])[:16]
}

// ErrEmptyField reports a cell field that the protocol requires to be non-empty.
var ErrEmptyField = errors.New("cell: required field is empty")

// New builds a cell and derives its ID. ts must already be an RFC3339 timestamp;
// the caller supplies it so that callers which need a deterministic cell (tests,
// replay, re-derivation from an existing record) can produce one.
func New(typ, from, ts, channel string, data map[string]Value, refs, tags []string, ttl int) (Cell, error) {
	for name, v := range map[string]string{"type": typ, "from": from, "ts": ts, "channel": channel} {
		if strings.TrimSpace(v) == "" {
			return Cell{}, fmt.Errorf("%w: %s", ErrEmptyField, name)
		}
	}
	if data == nil {
		data = map[string]Value{}
	}
	return Cell{
		ID:      DeriveID(typ, from, ts, channel, data),
		V:       Version,
		Type:    typ,
		From:    from,
		TS:      ts,
		Channel: channel,
		Data:    data,
		Refs:    refs,
		Tags:    tags,
		TTL:     ttl,
	}, nil
}

// Marshal renders the cell as one JSONL line, with no trailing newline.
// Field order is fixed so that a cell round-trips to identical bytes.
func (c Cell) Marshal() string {
	var b strings.Builder
	b.WriteByte('{')
	writeJSONString(&b, "id")
	b.WriteByte(':')
	writeJSONString(&b, c.ID)
	for _, kv := range []struct {
		k string
		v Value
	}{
		{"v", I(c.V)},
		{"type", S(c.Type)},
		{"from", S(c.From)},
		{"ts", S(c.TS)},
		{"channel", S(c.Channel)},
		{"data", O(c.Data)},
		{"refs", strsToArray(c.Refs)},
		{"ttl", I(c.TTL)},
		{"tags", strsToArray(c.Tags)},
	} {
		b.WriteByte(',')
		writeJSONString(&b, kv.k)
		b.WriteByte(':')
		kv.v.writeCanonical(&b)
	}
	b.WriteByte('}')
	return b.String()
}

func strsToArray(ss []string) A {
	out := make(A, 0, len(ss))
	for _, s := range ss {
		out = append(out, S(s))
	}
	return out
}
