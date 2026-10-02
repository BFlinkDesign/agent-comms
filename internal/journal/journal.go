// Package journal stores host-attributed records as an append-only file per host.
//
// One file per writer is the whole concurrency design. Several machines publish
// into the same repository, and if they shared a file every push would be a merge
// conflict on the same trailing lines. Partitioning by host means no two writers
// ever touch the same path, so git never has to merge journal content at all --
// a rename or a delete can still conflict, but ordinary appends cannot.
//
// Records are HIVE cells, so their IDs are content-derived. That is what makes a
// duplicate detectable: if the same observation is published twice, from a retry
// or from two hosts seeing the same fact, the two records carry the same ID and a
// reader can collapse them. A random ID would make that impossible.
//
// Durability is deliberately modest and deliberately honest. Append is a single
// write to a file opened O_APPEND, which POSIX guarantees is atomic for a write
// this small, so a concurrent writer can interleave whole lines but never
// characters. A crash between the write and the filesystem flushing can still
// leave a torn final line, so Read reports a trailing partial record as a
// recoverable defect instead of silently dropping it -- a journal that quietly
// loses its last entry after a crash is worse than one that says it did.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BFlinkDesign/agent-comms/internal/cell"
)

// MaxRecordBytes bounds one record, newline included. The append-atomicity
// guarantee this package relies on holds only for writes the kernel does not
// split, so a record that would exceed this is refused at the boundary rather
// than written and later found interleaved.
const MaxRecordBytes = 4096

// nameRe constrains the host-file stem. It matches the channel-name rule the rest
// of this bus enforces, so a journal file can never be named in a way that
// escapes its directory or collides with a channel.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Errors callers are expected to distinguish.
var (
	// ErrBadName reports a host stem that is not a safe single path segment.
	ErrBadName = errors.New("journal: unsafe host file name")
	// ErrTooLarge reports a record above MaxRecordBytes.
	ErrTooLarge = errors.New("journal: record exceeds the maximum size")
	// ErrSymlink reports that the target path is a symbolic link.
	ErrSymlink = errors.New("journal: refusing to write through a symlink")
	// ErrTornRecord reports a trailing partial line, i.e. an interrupted append.
	ErrTornRecord = errors.New("journal: trailing partial record")
	// ErrNoStore reports that the journal directory does not exist. A read says
	// this rather than inventing an empty store, so a mistyped path is
	// distinguishable from a machine that has genuinely recorded nothing.
	ErrNoStore = errors.New("journal: store directory does not exist")
)

// Store is a directory of per-host journal files.
type Store struct{ dir string }

// Open resolves dir as a journal store. It does NOT create the directory:
// creation happens in Append, when there is actually something to write.
//
// That split matters for a read. When Open created its target unconditionally, a
// mistyped --dir produced an empty store, answered "no records", exited zero and
// left a stray directory behind — an empty answer indistinguishable from a wrong
// path, which for a tool whose whole purpose is answering "which machine did
// that" is the worst output available. Reads now report ErrNoStore instead.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("journal: resolving %q: %w", dir, err)
	}
	return &Store{dir: abs}, nil
}

// Dir reports the resolved store directory.
func (s *Store) Dir() string { return s.dir }

// FileName maps a host ID to its journal file stem. Host IDs are of the form
// "host:<hex>", which is not a legal file name on Windows, so the separator is
// replaced rather than the value altered.
func FileName(hostID string) string {
	stem := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(hostID)), ":", "-")
	return stem
}

// path validates the stem and returns the absolute file path. Validating the stem
// against a strict pattern is what makes traversal impossible: no accepted stem
// can contain a separator or a dot segment, so the join cannot escape the store.
func (s *Store) path(stem string) (string, error) {
	if !nameRe.MatchString(stem) {
		return "", fmt.Errorf("%w: %q", ErrBadName, stem)
	}
	return filepath.Join(s.dir, stem+".jsonl"), nil
}

// Append writes one cell as a single line to the host's journal file.
//
// The write is one syscall on a file opened for append, so two processes writing
// concurrently produce interleaved whole records rather than a corrupted line.
// The file is fsynced before returning: this journal exists so that a machine can
// say what it did, and a record that is lost to a power failure is a record that
// never existed.
func (s *Store) Append(hostID string, c cell.Cell) error {
	stem := FileName(hostID)
	p, err := s.path(stem)
	if err != nil {
		return err
	}

	line := c.Marshal() + "\n"
	if len(line) > MaxRecordBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(line), MaxRecordBytes)
	}
	if strings.Count(line, "\n") != 1 {
		// Marshal escapes newlines, so this is unreachable via the public API.
		// It is checked anyway because one stray newline would silently split a
		// record into two malformed ones and corrupt every reader downstream.
		return fmt.Errorf("journal: record contains an embedded newline: %q", line)
	}

	// Refuse a symlinked target. On unix openAppend additionally passes O_NOFOLLOW,
	// which closes the window between this check and the open; on Windows the
	// check alone is what is available, so the residual race is documented rather
	// than hidden.
	if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlink, p)
	}

	// The store directory is created here rather than in Open, so that a read
	// against a mistyped path fails instead of silently manufacturing an empty
	// store. Whether this call created the file decides whether the directory
	// needs syncing below.
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("journal: creating %s: %w", s.dir, err)
	}
	_, statErr := os.Lstat(p)
	isNew := errors.Is(statErr, os.ErrNotExist)

	// A crash partway through an earlier append leaves a fragment with no
	// newline. Joined to it, this record would be lost with it, and published
	// that way. It starts a line of its own instead, in the same single write, so
	// the fragment stays one malformed line that readers report. A concurrent
	// appender writes whole lines, so it can only make this add an empty line.
	if !isNew && !endsWithNewline(p) {
		line = "\n" + line
	}

	f, err := openAppend(p)
	if err != nil {
		return fmt.Errorf("journal: opening %s: %w", p, err)
	}
	defer f.Close()

	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("journal: appending to %s: %w", p, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("journal: syncing %s: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("journal: closing %s: %w", p, err)
	}

	if isNew {
		// Syncing the file persists its data and inode but not the directory
		// entry that names it. Without this, power loss just after the first
		// record on a fresh machine leaves the journal file absent rather than
		// merely short -- the record would not be truncated, it would never have
		// existed, which is precisely what the package doc promises cannot
		// happen. Only needed when the entry is new; later appends do not change
		// the directory.
		if err := syncDir(s.dir); err != nil {
			return fmt.Errorf("journal: syncing directory %s: %w", s.dir, err)
		}
	}
	return nil
}

// endsWithNewline reports whether a file is empty or ends with a newline. A file
// that cannot be read is taken to be whole: the append that follows fails on it
// anyway, or succeeds as before.
func endsWithNewline(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return true
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return true
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return true
	}
	return last[0] == '\n'
}

// Record is one parsed journal line, kept as raw text plus the fields a reader
// needs to order and attribute it. The raw line is retained so that a record
// written by a newer version, carrying fields this build does not know about, can
// still be relayed without being silently truncated.
type Record struct {
	ID      string
	Type    string
	From    string
	TS      string
	Channel string
	Raw     string
	// Host is the journal file the record was read from, which is the attribution
	// git could never provide.
	Host string
	// Line is the 1-indexed position in its file, so a defect can be pointed at.
	Line int
}

// Read returns every record in one host's journal, in file order.
//
// A trailing partial line is returned as a wrapped ErrTornRecord alongside the
// records that were read successfully. Callers that can proceed on a truncated
// journal should use the records and report the error; callers that cannot should
// treat it as fatal. Either way the damage is visible.
func (s *Store) Read(hostID string) ([]Record, error) {
	stem := FileName(hostID)
	p, err := s.path(stem)
	if err != nil {
		return nil, err
	}
	return s.readFile(p, stem)
}

// Hosts lists the host stems present in the store, in lexical order.
func (s *Store) Hosts() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoStore, s.dir)
		}
		return nil, fmt.Errorf("journal: listing %s: %w", s.dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".jsonl")
		if nameRe.MatchString(stem) {
			out = append(out, stem)
		}
	}
	return out, nil
}

// ReadAll returns every record from every host. The per-host error, if any, is
// returned joined, so one damaged journal does not hide the others' contents:
// reporting nine good hosts and naming the tenth as torn is strictly more useful
// than failing the whole read.
func (s *Store) ReadAll() ([]Record, error) {
	hosts, err := s.Hosts()
	if err != nil {
		return nil, err
	}
	var all []Record
	var errs []error
	for _, stem := range hosts {
		recs, rerr := s.readFile(filepath.Join(s.dir, stem+".jsonl"), stem)
		all = append(all, recs...)
		if rerr != nil {
			errs = append(errs, rerr)
		}
	}
	return all, errors.Join(errs...)
}

// ErrMalformed reports a line that is present and terminated but not a usable
// record. It is distinct from ErrTornRecord: a torn record is an interrupted
// write, a malformed one is a write that completed and was wrong.
var ErrMalformed = errors.New("journal: malformed record")

// parseRecord reads the fields a journal reader needs, and keeps the original
// text. Unknown fields are preserved in Raw rather than dropped, so a record
// written by a newer build can still be read and relayed by an older one.
func parseRecord(line string) (Record, error) {
	var envelope struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		From    string `json:"from"`
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	// A record with no id cannot be deduplicated and a record with no timestamp
	// cannot be placed, so neither is usable; say which is missing. The fields are
	// checked in a fixed order rather than by ranging a map, because Go randomises
	// map iteration and an operator diagnosing a damaged journal would otherwise
	// be told a different cause each time they re-ran the same command.
	for _, f := range []struct{ name, value string }{
		{"id", envelope.ID}, {"ts", envelope.TS}, {"from", envelope.From},
	} {
		if strings.TrimSpace(f.value) == "" {
			return Record{}, fmt.Errorf("%w: required field %q is empty", ErrMalformed, f.name)
		}
	}
	return Record{
		ID:      envelope.ID,
		Type:    envelope.Type,
		From:    envelope.From,
		TS:      envelope.TS,
		Channel: envelope.Channel,
		Raw:     line,
	}, nil
}

func (s *Store) readFile(p, stem string) ([]Record, error) {
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("journal: opening %s: %w", p, err)
	}
	defer f.Close()

	var out []Record
	// Defects are collected rather than returned at the first one. A malformed
	// line in the middle of a file must not hide the records after it: this bus
	// is explicitly open to any process that can append, including the raw uuid4
	// plane whose records this parser rejects, so a line it cannot read is an
	// expected input rather than proof the file is ruined. Returning early there
	// made `fleetd where` report a stale last-activity and an undercount with no
	// signal beyond a warning -- the exact "silently guesses" failure this package
	// is written to avoid.
	var defects []error
	r := bufio.NewReaderSize(f, MaxRecordBytes)
	for n := 1; ; n++ {
		line, rerr := r.ReadString('\n')
		complete := strings.HasSuffix(line, "\n")
		trimmed := strings.TrimRight(line, "\r\n")

		if trimmed != "" && complete {
			rec, perr := parseRecord(trimmed)
			if perr != nil {
				defects = append(defects, fmt.Errorf("journal: %s line %d: %w", p, n, perr))
			} else {
				rec.Host, rec.Line = stem, n
				out = append(out, rec)
			}
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if trimmed != "" && !complete {
					// The last append did not finish. Say so, and hand back
					// everything that did.
					defects = append(defects, fmt.Errorf("journal: %s line %d: %w: %d bytes with no terminator",
						p, n, ErrTornRecord, len(trimmed)))
				}
				return out, errors.Join(defects...)
			}
			defects = append(defects, fmt.Errorf("journal: reading %s line %d: %w", p, n, rerr))
			return out, errors.Join(defects...)
		}
	}
}
