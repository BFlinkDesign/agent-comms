package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/cell"
	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

// A machine that records before it knows the fleet's salt files its records
// under another identity: a hook that fired before `fleetd init`, one started
// with a FLEET_SALT the journal does not use, or one that read a salt the
// journal has since replaced. Nothing would ever publish those records, so every
// sync, while it holds its lock, files them under the machine's fleet identity:
// it moves each such file into the clone's git directory, then files the records
// of the moved copy that the journal does not already hold under that identity.
// A file git tracks was published under its identity up to some point; it is put
// back as published, and only the records after that are filed.

// settleTime is how long the journal's fleetd.json must have been in place
// before records are re-filed, so that a hook which resolved its salt just
// before it appeared has finished appending.
const settleTime = 2 * time.Second

// refileSettle is settleTime. It is a variable only so that tests, which set a
// journal up and file its records at once, need not wait it out every time.
var refileSettle = settleTime

// preInitDir is where, inside the clone's git directory, each moved file is
// kept. It is never committed.
const preInitDir = "fleetd-pre-init"

// refiledName is the file, in preInitDir, that holds each moved copy's size when
// its records were last filed. A process that had a file open when it was moved
// appends to the moved copy, so a copy that has grown is read again.
const refiledName = "refiled.json"

// movedCopy matches a moved file's name: a journal file's, then when it moved.
var movedCopy = regexp.MustCompile(`^(host-[a-z0-9_-]{1,59})\.jsonl\.[0-9]+$`)

// maxSalts bounds the salts one pass derives an identity from: each costs a
// derivation, which on macOS starts a process, and a look at git's copy of a
// file. The notes hold a handful; one written full of salts must not hold up
// every sync. The salts noted first are the ones kept.
const maxSalts = 64

// otherIdentities lists this machine under each salt it may have recorded with
// besides the fleet's: none, FLEET_SALT, every salt noted in the clone's git
// directory as one the journal no longer uses, and every salt noted beside the
// journal directory as one a record was written under before it had fleetd.json.
func otherIdentities(dir, gitDir string, me hostOut) []hostOut {
	var out []hostOut
	seen, ids := map[string]bool{}, map[string]bool{me.ID: true}
	salts := append([]string{"", os.Getenv("FLEET_SALT")}, gitsync.PastSalts(gitDir)...)
	for _, salt := range append(salts, notedSalts(dir)...) {
		if seen[salt] {
			continue
		}
		if len(seen) == maxSalts {
			break
		}
		seen[salt] = true
		if h := identity(salt); !ids[h.ID] {
			ids[h.ID] = true
			out = append(out, h)
		}
	}
	return out
}

// saltsNote is the file, beside the journal directory, that notes each salt a
// record was written under for want of the journal's fleetd.json: --salt or
// FLEET_SALT. Once the journal has fleetd.json, re-filing finds those records by
// it, whatever the environment of the sync that files them.
// A journal directory at the root of a filesystem has nothing beside it, and gets
// no note: saltsNote is then empty.
func saltsNote(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if filepath.Dir(dir) == dir {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".salts")
}

// noteSalt adds salt to dir's salts note, as gitsync.AppendSalt does, and warns
// when it cannot.
func noteSalt(dir, salt string, warn io.Writer) {
	path := saltsNote(dir)
	err := errors.New("a journal directory at the root of a filesystem has nothing beside it")
	if path != "" {
		err = gitsync.AppendSalt(path, salt)
	}
	if err != nil {
		fmt.Fprintf(warn, "fleetd: warning: could not note this record's salt beside %s (%v); once the journal has %s, "+
			"only a sync with the same salt in FLEET_SALT files it under the fleet's\n", dir, err, gitsync.FleetFile)
	}
}

// notedSalts returns the salts dir's salts note holds.
func notedSalts(dir string) []string {
	if path := saltsNote(dir); path != "" {
		return gitsync.ReadSalts(path)
	}
	return nil
}

// copyAt returns name's content at rev. ok is false when rev has no such file;
// err is set when git could not say, and the caller then leaves the file alone.
func copyAt(ctx context.Context, dir, rev, name string) (content string, ok bool, err error) {
	entry, err := gitsync.Git(ctx, dir, nil, "ls-tree", "-z", rev, "--", name)
	if err != nil || entry == "" {
		return "", false, err
	}
	content, err = gitsync.Git(ctx, dir, nil, "cat-file", "blob", rev+":"+name)
	return content, err == nil, err
}

// movable reports whether another identity's journal file holds records not
// published under it: git does not track it, or it differs from git's copy. A
// file git cannot answer for is left for the next pass.
func movable(ctx context.Context, dir, name string) bool {
	if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	tracked, err := gitsync.Git(ctx, dir, nil, "ls-files", "-z", "--", name)
	if err != nil {
		return false
	}
	if tracked == "" {
		return true
	}
	differs, err := gitsync.Git(ctx, dir, nil, "diff", "--name-only", "-z", "--", name)
	return err == nil && differs != ""
}

// refile files under me the records this machine wrote under another identity,
// as described above. It does nothing until dir holds the journal's fleetd.json,
// me is the identity it gives, and it has been in place for refileSettle. It
// returns how many records it appended. The caller holds the sync lock, and ctx
// bounds every git command it runs.
func refile(ctx context.Context, dir, gitDir string, me hostOut, warn io.Writer) (int, error) {
	fleet, ok, err := gitsync.ReadFleet(dir)
	if err != nil || !ok || identity(fleet.Salt).ID != me.ID {
		return 0, nil
	}
	info, err := os.Stat(filepath.Join(dir, gitsync.FleetFile))
	if err != nil || time.Since(info.ModTime()) < refileSettle {
		return 0, nil
	}
	others := otherIdentities(dir, gitDir, me)
	putBackDeleted(ctx, dir, gitDir, me, others)
	keep := filepath.Join(gitDir, preInitDir)
	moved := map[string]bool{}
	for _, o := range others {
		name := journal.FileName(o.ID) + ".jsonl"
		if !movable(ctx, dir, name) {
			continue
		}
		dests, err := moveAside(ctx, dir, gitDir, keep, name, "HEAD")
		for _, dest := range dests {
			moved[filepath.Base(dest)] = true
		}
		if err != nil {
			return 0, err
		}
	}
	return fileMoved(ctx, dir, keep, me, moved, warn)
}

// putBackDeleted puts back, as git has it, a journal file of this machine's that
// git tracks and the work tree lacks: a pass killed between moving a file and
// putting its published copy back leaves it so. This machine's own file is put
// back as the remote has it, since a sync publishes only what follows that.
func putBackDeleted(ctx context.Context, dir, gitDir string, me hostOut, others []hostOut) {
	for _, h := range append([]hostOut{me}, others...) {
		name := journal.FileName(h.ID) + ".jsonl"
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		rev := "HEAD"
		if h.ID == me.ID {
			rev = "@{upstream}"
		}
		if content, ok, err := copyAt(ctx, dir, rev, name); err == nil && ok {
			if staged, err := stage(gitDir, content); err == nil {
				_, _ = putBack(staged, filepath.Join(dir, name), content)
				os.Remove(staged)
			}
		}
	}
}

// moveAside moves dir's file name into keep, and puts back the copy rev has, if
// it has one, so the clone keeps that identity's published records. The copy is
// read and staged before the move, so the file is back an instant after it
// leaves. A process that starts the file again in that instant has its new file
// moved too, at most three times in all. It returns where the files went.
func moveAside(ctx context.Context, dir, gitDir, keep, name, rev string) ([]string, error) {
	published, ok, err := copyAt(ctx, dir, rev, name)
	if err != nil {
		return nil, err
	}
	var staged string
	if ok {
		if staged, err = stage(gitDir, published); err != nil {
			return nil, err
		}
		defer os.Remove(staged)
	}
	if err := os.MkdirAll(keep, 0o700); err != nil {
		return nil, err
	}
	var dests []string
	for range 3 {
		dest, err := freeName(keep, name)
		if err != nil {
			return dests, err
		}
		if err := gitsync.RenameRetry(ctx, filepath.Join(dir, name), dest); err != nil {
			return dests, err
		}
		dests = append(dests, dest)
		if !ok {
			return dests, nil
		}
		placed, err := putBack(staged, filepath.Join(dir, name), published)
		if placed || err != nil {
			return dests, err
		}
	}
	return dests, nil
}

// now is time.Now; a test fixes it to give two moves the same time, as Windows'
// clock can, whose ticks are milliseconds apart.
var now = time.Now

// freeName names a copy of name moved into keep now: the time, made unique
// among the copies keep holds, since a rename onto an existing name would replace
// that copy. The caller holds the sync lock, so nothing else adds to keep.
func freeName(keep, name string) (string, error) {
	t := now().UnixNano()
	for i := range int64(1000) {
		dest := filepath.Join(keep, fmt.Sprintf("%s.%d", name, t+i))
		_, err := os.Lstat(dest)
		if errors.Is(err, os.ErrNotExist) {
			return dest, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s holds a thousand copies of %s moved at %d", keep, name, t)
}

// stage writes content to a temporary file in gitDir, which is on the journal
// directory's filesystem, for putBack.
func stage(gitDir, content string) (string, error) {
	f, err := os.CreateTemp(gitDir, "fleetd-put-back-*")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// linkFile and createFile are putBack's two ways to make a file; tests replace
// them to take its other path, and to append a record in its window.
var (
	linkFile   = os.Link
	createFile = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	}
)

// putBack makes path hold content, from the staged copy, only if path does not
// exist: a process appending under that identity may have started the file again,
// and its record must not be overwritten. The file appears whole, through a hard
// link to the staged copy. Where the filesystem has no hard links it is created
// exclusively and appended to, so a record a process appends at the same moment
// is kept, after or before the copy. placed says whether it put the copy back.
func putBack(staged, path, content string) (placed bool, err error) {
	err = linkFile(staged, path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrExist):
		return false, nil
	}
	f, err := createFile(path)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err == nil, err
}

// fileMoved files under me the records of every copy in keep that has grown
// since its records were last filed: those just moved, and earlier ones a
// process appended to after they moved. A record already filed, or published
// under its own identity, here or on the remote, is skipped, so reading a copy
// again adds nothing twice. A copy git cannot say what was published for waits
// for the next pass.
func fileMoved(ctx context.Context, dir, keep string, me hostOut, moved map[string]bool, warn io.Writer) (int, error) {
	entries, err := os.ReadDir(keep)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sizes := map[string]int64{}
	if data, err := os.ReadFile(filepath.Join(keep, refiledName)); err == nil {
		_ = json.Unmarshal(data, &sizes)
	}
	var grown []string
	for _, e := range entries {
		info, err := e.Info()
		if movedCopy.MatchString(e.Name()) && err == nil && info.Mode().IsRegular() && sizes[e.Name()] != info.Size() {
			grown = append(grown, e.Name())
		}
	}
	if len(grown) == 0 {
		return 0, nil
	}
	store, err := journal.Open(dir)
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	recs, _ := store.Read(me.ID)
	for _, r := range recs {
		have[r.ID] = true
	}
	total := 0
	for _, copyName := range grown {
		m := movedCopy.FindStringSubmatch(copyName)
		from := strings.Replace(m[1], "-", ":", 1)
		published, known := publishedIDs(ctx, dir, m[1]+".jsonl")
		if !known {
			continue
		}
		n, read, skipped, err := refileOne(ctx, store, filepath.Join(keep, copyName), from, me, have, published)
		total += n
		if err != nil {
			return total, err
		}
		sizes[copyName] = read
		if skipped > 0 && moved[copyName] {
			fmt.Fprintf(warn, "fleetd: warning: %s of %s could not be filed under this machine's fleet identity; "+
				"the file is kept as %s\n", plural(skipped, "record"), m[1]+".jsonl", filepath.Join(keep, copyName))
		}
	}
	data, err := json.Marshal(sizes)
	if err != nil {
		return total, err
	}
	path := filepath.Join(keep, refiledName)
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0o600); err != nil {
		return total, err
	}
	return total, gitsync.RenameRetry(ctx, path+".tmp", path)
}

// publishedIDs returns the ids of the records name holds in this clone's HEAD
// and on the remote. known is false when git could not say.
func publishedIDs(ctx context.Context, dir, name string) (ids map[string]bool, known bool) {
	ids = map[string]bool{}
	for _, rev := range []string{"HEAD", "@{upstream}"} {
		content, _, err := copyAt(ctx, dir, rev, name)
		if err != nil {
			return nil, false
		}
		for _, line := range lines([]byte(content)) {
			var r struct{ ID string }
			if json.Unmarshal([]byte(line), &r) == nil {
				ids[r.ID] = true
			}
		}
	}
	return ids, true
}

// refileOne appends to me's file, through store, every complete record in path
// that is not in published and that have lacks once filed under me, and adds it
// to have. A line that is not a record of identity from is skipped and counted.
// read is how many bytes of path it read; a final line without a newline may
// still be being written, and is read again once the file grows. Once ctx ends it
// stops, reporting nothing read.
func refileOne(ctx context.Context, store *journal.Store, path, from string, me hostOut, have, published map[string]bool) (added int, read int64, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range lines(data[:bytes.LastIndexByte(data, '\n')+1]) {
		// Each record costs an fsync, so a long backlog is bounded by the sync's
		// time. What is left is read again next time; have keeps what this pass
		// filed from being filed twice.
		if err := ctx.Err(); err != nil {
			return added, 0, skipped, err
		}
		var r struct{ ID string }
		if json.Unmarshal([]byte(line), &r) == nil && published[r.ID] {
			continue
		}
		c, err := reencode(line, from, me.ID, true)
		if err != nil {
			skipped++
			continue
		}
		if have[c.ID] {
			continue
		}
		err = store.Append(me.ID, c)
		if errors.Is(err, journal.ErrTooLarge) {
			// refiled.from took it past the limit. Unmarked it is the size it was
			// written with, so it fits; `where` merely cannot tell it was filed late.
			c, _ = reencode(line, from, me.ID, false)
			if have[c.ID] {
				continue
			}
			err = store.Append(me.ID, c)
		}
		if errors.Is(err, journal.ErrTooLarge) {
			skipped++
			continue
		}
		if err != nil {
			return added, 0, skipped, err
		}
		have[c.ID] = true
		added++
	}
	return added, int64(len(data)), skipped, nil
}

// reencode rebuilds a record of host id from as one of host id to: the same
// record with data's host.id replaced and, if mark is set, refiled.from naming
// the identity it was written under, so that `where` can tell it was filed late.
// A record of to itself comes back unchanged. Only values a journal record holds
// are accepted: strings, booleans and integers.
func reencode(line, from, to string, mark bool) (cell.Cell, error) {
	var rec struct {
		Type    string                     `json:"type"`
		From    string                     `json:"from"`
		TS      string                     `json:"ts"`
		Channel string                     `json:"channel"`
		Data    map[string]json.RawMessage `json:"data"`
		Refs    []string                   `json:"refs"`
		TTL     int                        `json:"ttl"`
		Tags    []string                   `json:"tags"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return cell.Cell{}, err
	}
	var hostID string
	if err := json.Unmarshal(rec.Data["host.id"], &hostID); err != nil || hostID != from {
		return cell.Cell{}, errors.New("not a record of that identity")
	}
	data := make(map[string]cell.Value, len(rec.Data)+1)
	for k, raw := range rec.Data {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return cell.Cell{}, err
		}
		switch x := v.(type) {
		case string:
			data[k] = cell.S(x)
		case bool:
			data[k] = cell.B(x)
		case json.Number:
			n, err := x.Int64()
			if err != nil {
				return cell.Cell{}, fmt.Errorf("data %q is not an integer", k)
			}
			data[k] = cell.I(n)
		default:
			return cell.Cell{}, fmt.Errorf("data %q is not a string, boolean or integer", k)
		}
	}
	if from != to {
		data["host.id"] = cell.S(to)
		if mark {
			data[refiledFrom] = cell.S(from)
		}
	}
	return cell.New(rec.Type, rec.From, rec.TS, rec.Channel, data, rec.Refs, rec.Tags, rec.TTL)
}

// refiledFrom is the data field a re-filed record carries: the host id it was
// first written under.
const refiledFrom = "refiled.from"

// reconcileOwn puts back this machine's published records when its own file no
// longer starts with the remote's copy: its journal directory was set up again,
// or restored from an older copy, over records written since. The same mismatch
// is what another machine with this machine's id produces, and only a person can
// tell the two apart, so only `fleetd init --reclaim` calls it, holding the sync
// lock. The local file moves aside, the remote's copy takes its place, and refile
// then files the moved records the remote lacks after it.
func reconcileOwn(ctx context.Context, dir, gitDir string, me hostOut) error {
	name := journal.FileName(me.ID) + ".jsonl"
	published, ok, err := copyAt(ctx, dir, "@{upstream}", name)
	if err != nil || !ok {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil // a missing file is put back by refile
	}
	if bytes.HasPrefix(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte(published)) {
		return nil
	}
	_, err = moveAside(ctx, dir, gitDir, filepath.Join(gitDir, preInitDir), name, "@{upstream}")
	return err
}
