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
	"slices"
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

// refileSettle is how long the journal's fleetd.json must have been in place
// before records are re-filed, so that a hook which resolved its salt just
// before it appeared has finished appending.
const refileSettle = 2 * time.Second

// preInitDir is where, inside the clone's git directory, each moved file is
// kept. It is never committed.
const preInitDir = "fleetd-pre-init"

// refiledName is the file, in preInitDir, that holds each moved copy's size when
// its records were last filed. A process that had a file open when it was moved
// appends to the moved copy, so a copy that has grown is read again.
const refiledName = "refiled.json"

// movedCopy matches a moved file's name: a journal file's, then when it moved.
var movedCopy = regexp.MustCompile(`^(host-[a-z0-9_-]{1,59})\.jsonl\.[0-9]+$`)

// otherIdentities lists this machine under each salt it may have recorded with
// besides the fleet's: none, FLEET_SALT, and every salt noted in the clone's git
// directory as one the journal no longer uses.
func otherIdentities(gitDir string, me hostOut) []hostOut {
	var out []hostOut
	for _, salt := range append([]string{"", os.Getenv("FLEET_SALT")}, gitsync.PastSalts(gitDir)...) {
		h := identity(salt)
		if h.ID == me.ID || slices.ContainsFunc(out, func(o hostOut) bool { return o.ID == h.ID }) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// refilePending reports whether dir holds a file refile would move.
func refilePending(dir string, me hostOut) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gitDir, err := gitsync.Git(ctx, dir, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}
	for _, o := range otherIdentities(strings.TrimSpace(gitDir), me) {
		if movable(ctx, dir, journal.FileName(o.ID)+".jsonl") {
			return true
		}
	}
	return false
}

// movable reports whether a journal file of another identity holds records not
// published under it: it is a regular file git does not track, or one that
// differs from git's copy.
func movable(ctx context.Context, dir, name string) bool {
	if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if _, err := gitsync.Git(ctx, dir, nil, "ls-files", "--error-unmatch", "--", name); err != nil {
		return true
	}
	_, err := gitsync.Git(ctx, dir, nil, "diff", "--quiet", "--", name)
	return err != nil
}

// refile files under me the records this machine wrote under another identity,
// as described above. It does nothing until dir holds the journal's fleetd.json,
// me is the identity it gives, and it has been in place for refileSettle. It
// returns how many records it appended. The caller holds the sync lock.
func refile(dir, gitDir string, me hostOut, warn io.Writer) (int, error) {
	fleet, ok, err := gitsync.ReadFleet(dir)
	if err != nil || !ok || identity(fleet.Salt).ID != me.ID {
		return 0, nil
	}
	info, err := os.Stat(filepath.Join(dir, gitsync.FleetFile))
	if err != nil || time.Since(info.ModTime()) < refileSettle {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	keep := filepath.Join(gitDir, preInitDir)
	moved := map[string]bool{}
	for _, o := range otherIdentities(gitDir, me) {
		name := journal.FileName(o.ID) + ".jsonl"
		if !movable(ctx, dir, name) {
			continue
		}
		dest, err := moveAside(ctx, dir, keep, name)
		if err != nil {
			return 0, err
		}
		moved[filepath.Base(dest)] = true
	}
	return fileMoved(ctx, dir, keep, me, moved, warn)
}

// moveAside moves dir's file name into keep. One git tracks is put back as git
// has it, so the clone keeps that identity's published records. It returns
// where the file went.
func moveAside(ctx context.Context, dir, keep, name string) (string, error) {
	if err := os.MkdirAll(keep, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(keep, fmt.Sprintf("%s.%d", name, time.Now().UnixNano()))
	if err := gitsync.RenameRetry(filepath.Join(dir, name), dest); err != nil {
		return "", err
	}
	if published, err := gitsync.Git(ctx, dir, nil, "cat-file", "blob", "HEAD:"+name); err == nil {
		if err := createOnly(dir, name, []byte(published)); err != nil {
			return dest, err
		}
	}
	return dest, nil
}

// createOnly writes content to dir's file name only if there is none: a process
// still appending under that identity may have started the file again since it
// was moved, and its record must not be overwritten. The file appears whole or
// not at all, through a hard link to a finished temporary file; where hard links
// are not available it is created exclusively and written.
func createOnly(dir, name string, content []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(filepath.Dir(dir), "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	err = os.Link(tmp.Name(), path)
	switch {
	case err == nil, errors.Is(err, os.ErrExist):
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// fileMoved files under me the records of every copy in keep that has grown
// since its records were last filed: those just moved, and earlier ones a
// process appended to after they moved. A record already filed, or published
// under its own identity, is skipped, so reading a copy again adds nothing twice.
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
		published := map[string]bool{}
		if blob, err := gitsync.Git(ctx, dir, nil, "cat-file", "blob", "HEAD:"+m[1]+".jsonl"); err == nil {
			for _, line := range lines([]byte(blob)) {
				var r struct{ ID string }
				if json.Unmarshal([]byte(line), &r) == nil {
					published[r.ID] = true
				}
			}
		}
		n, read, skipped, err := refileOne(store, filepath.Join(keep, copyName), from, me, have, published)
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
	return total, gitsync.RenameRetry(path+".tmp", path)
}

// refileOne appends to me's file, through store, every complete record in path
// that is not in published and that have lacks once filed under me, and adds it
// to have. A line that is not a record of identity from is skipped and counted.
// read is how many bytes of path it read; a final line without a newline may
// still be being written, and is read again once the file grows.
func refileOne(store *journal.Store, path, from string, me hostOut, have, published map[string]bool) (added int, read int64, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range lines(data[:bytes.LastIndexByte(data, '\n')+1]) {
		var r struct{ ID string }
		if json.Unmarshal([]byte(line), &r) == nil && published[r.ID] {
			continue
		}
		c, err := reencode(line, from, me.ID)
		if err != nil {
			skipped++
			continue
		}
		if have[c.ID] {
			continue
		}
		if err := store.Append(me.ID, c); err != nil {
			return added, 0, skipped, err
		}
		have[c.ID] = true
		added++
	}
	return added, int64(len(data)), skipped, nil
}

// reencode rebuilds a record of host id from as one of host id to: the same
// record with data's host.id replaced, and refiled.from naming the identity it
// was written under, so that `where` can tell it was filed late. A record of to
// itself comes back unchanged. Only values a journal record holds are accepted:
// strings, booleans and integers.
func reencode(line, from, to string) (cell.Cell, error) {
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
		data[refiledFrom] = cell.S(from)
	}
	return cell.New(rec.Type, rec.From, rec.TS, rec.Channel, data, rec.Refs, rec.Tags, rec.TTL)
}

// refiledFrom is the data field a re-filed record carries: the host id it was
// first written under.
const refiledFrom = "refiled.from"

// reconcileOwn makes this machine's own journal file start with the copy git
// has, when the two disagree because the clone was set up afresh over a file
// written since: the journal directory was lost, a hook recorded, and init ran
// again. The local file moves aside, git's copy takes its place, and refile
// then files the moved records git's copy lacks. Only init calls it, holding the
// sync lock, and only when every record in git's copy names this machine;
// otherwise another machine derives the same id, which a sync reports.
func reconcileOwn(dir, gitDir string, me hostOut) error {
	name := journal.FileName(me.ID) + ".jsonl"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	published, err := gitsync.Git(ctx, dir, nil, "cat-file", "blob", "HEAD:"+name)
	if err != nil {
		return nil // git has no copy: nothing to disagree with
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	if bytes.HasPrefix(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte(published)) {
		return nil
	}
	for _, line := range lines([]byte(published)) {
		if fieldsOf(line)["host.name"] != me.Name {
			return nil
		}
	}
	_, err = moveAside(ctx, dir, filepath.Join(gitDir, preInitDir), name)
	return err
}
