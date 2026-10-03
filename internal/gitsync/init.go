package gitsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/journal"
)

// FleetFile is the journal repository's own settings file. It holds the fleet's
// salt, so every machine that clones the journal derives host ids the same way
// without each one having to be given the salt: a hook started without
// FLEET_SALT in its environment would otherwise file its records under a second
// id for the same machine.
const FleetFile = "fleetd.json"

// Fleet is FleetFile's content.
type Fleet struct {
	About string `json:"about,omitempty"`
	Salt  string `json:"salt"`
}

const fleetAbout = "fleetd derives every machine's host id with this salt. Changing it gives every machine a new id."

var (
	// ErrNotJournal means the repository holds files a fleet journal never has,
	// so it is probably the wrong repository.
	ErrNotJournal = errors.New("gitsync: the repository does not look like a fleet journal")
	// ErrSaltMismatch means a salt was given that differs from the journal's.
	ErrSaltMismatch = errors.New("gitsync: the salt given differs from the journal's fleetd.json")
	// ErrBadFleetFile means FleetFile exists but is not valid JSON, or holds no salt.
	ErrBadFleetFile = errors.New("gitsync: fleetd.json is not usable")
	// ErrNeedSalt means the journal holds records but no FleetFile, so the salt
	// its machines use cannot be known, and inventing one would give each of them
	// a second id.
	ErrNeedSalt = errors.New("gitsync: the journal holds records but no fleetd.json")
	// ErrOtherRemote means the journal directory is a clone of another repository.
	ErrOtherRemote = errors.New("gitsync: the journal directory is a clone of another repository")
	// ErrDirInUse means the journal directory holds something besides journal
	// files, which init leaves alone.
	ErrDirInUse = errors.New("gitsync: the journal directory holds files that are not journal records")
	// ErrNoDefaultBranch means the repository's default branch does not exist
	// while several other branches do, so the journal may be on any of them.
	ErrNoDefaultBranch = errors.New("gitsync: the repository's default branch does not exist")
)

// unnamedDefault is the branch init's clone of an empty repository is on when the
// remote does not say which branch is its default: git falls back to
// init.defaultBranch, which init sets to this name for the clone, so this
// machine's own default is never mistaken for the remote's.
const unnamedDefault = "fleetd-unnamed-default"

// hostFile is the name of a machine's journal file: journal.FileName of a host
// id, which fleetd makes "host:" and 16 hex digits.
var hostFile = regexp.MustCompile(`^host-[a-z0-9_-]{1,59}\.jsonl$`)

// ReadFleet reads FleetFile from a journal directory; ok is false when it has none.
func ReadFleet(dir string) (f Fleet, ok bool, err error) {
	path := filepath.Join(dir, FleetFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Fleet{}, false, nil
	}
	if err != nil {
		return Fleet{}, false, err
	}
	if f, err = parseFleet(data, path); err != nil {
		return Fleet{}, false, err
	}
	return f, true, nil
}

func parseFleet(data []byte, where string) (Fleet, error) {
	var f Fleet
	if err := json.Unmarshal(data, &f); err != nil {
		return Fleet{}, fmt.Errorf("%w: %s is not valid JSON (%v)", ErrBadFleetFile, where, err)
	}
	if strings.TrimSpace(f.Salt) == "" {
		return Fleet{}, fmt.Errorf("%w: %s holds no salt", ErrBadFleetFile, where)
	}
	return f, nil
}

// InitOptions says which journal repository to set up, and where.
type InitOptions struct {
	// URL is the journal repository.
	URL string
	// Dir is the journal directory. It may already be a clone of URL; otherwise
	// it may be absent, empty, or hold journal files written before init and a
	// FleetFile, and nothing else.
	Dir string
	// Salt is a salt the caller was given. A journal without FleetFile gets it;
	// a journal with FleetFile keeps its own.
	Salt string
	// Strict refuses a journal whose salt differs from Salt; otherwise the
	// journal's salt is used and SaltDiffers is set.
	Strict bool
	// Run runs git; nil means Git.
	Run Runner
}

// InitResult says what Init did.
type InitResult struct {
	// Started is set when the repository was empty and Init made its first commit.
	Started bool `json:"started"`
	// WroteFleetFile is set when Init committed FleetFile to the repository.
	WroteFleetFile bool `json:"wrote_fleet_file"`
	// Cloned is set when Dir was not a clone, or was one with no commit, and now
	// follows the journal.
	Cloned bool `json:"cloned"`
	// Branch is the branch the clone follows.
	Branch string `json:"branch"`
	// Head is the remote commit Init found FleetFile in.
	Head string `json:"head"`
	// Restored lists tracked files the work tree lacked, which Init checked out.
	Restored []string `json:"restored,omitempty"`
	// Salt is the journal's salt. It is not a credential, but it is nobody's
	// business either, so it is not printed.
	Salt string `json:"-"`
	// SaltDiffers is set when a Salt was given, Strict was not, and the
	// journal's differs from it.
	SaltDiffers bool `json:"-"`
}

// Init makes Dir a clone of the journal repository that holds FleetFile.
//
// It never renames, rewrites or deletes Dir or a record in it, because a hook may
// be appending to one at any moment. A directory that is not a clone yet keeps
// its files where they are: Init clones without a work tree into a temporary
// directory beside it, makes sure the repository has FleetFile there, writes
// FleetFile into Dir so that every record from then on uses the journal's salt,
// and only then moves the clone's git directory into Dir and checks out the
// tracked files Dir lacks, overwriting none. A directory that is already a clone
// has FleetFile added to its repository the same way, with plumbing; then the
// journal's FleetFile replaces the one in its work tree, if they differ, and the
// tracked files it lacks are checked out. The rest of its work tree is left to
// the next sync. A clone with no commit, such as a plain clone of the repository
// made while it was empty, is set up in place, on the branch it is on.
//
// A repository without FleetFile gets it in a commit of its own, pushed at once;
// an empty repository gets it as its first commit. One that already holds
// records needs the salt its machines use, and one with files a journal never
// has is refused, both before anything is pushed. When another machine pushes
// first, Init takes what that machine pushed and looks again, at most
// MaxAttempts times. Running Init again finishes what an interrupted one began.
func Init(ctx context.Context, o InitOptions) (InitResult, error) {
	run := o.Run
	if run == nil {
		run = Git
	}
	url := o.URL
	// A local path is resolved here: git would resolve a relative one against the
	// directory it clones from, which is not the one the person typed it in.
	if info, err := os.Stat(url); err == nil && info.IsDir() {
		if abs, err := filepath.Abs(url); err == nil {
			url = abs
		}
	}
	if _, err := os.Lstat(filepath.Join(o.Dir, ".git")); err == nil {
		return initClone(ctx, o, url, run)
	}
	return initNew(ctx, o, url, run)
}

func initNew(ctx context.Context, o InitOptions, url string, run Runner) (InitResult, error) {
	var res InitResult
	if err := checkJournalDir(o.Dir); err != nil {
		return res, err
	}
	parent := filepath.Dir(o.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return res, err
	}
	removeAbandonedClones(o.Dir)
	tmp, err := os.MkdirTemp(parent, filepath.Base(o.Dir)+".init-")
	if err != nil {
		return res, err
	}
	// Init's own clone. Once its git directory has moved into Dir it is empty.
	defer os.RemoveAll(tmp)

	g := git{ctx: ctx, dir: parent, run: run}
	clone := append(batchSSH(g), "-c", "init.defaultBranch="+unnamedDefault, "clone", "--quiet", "--no-checkout", "--no-tags", "--", url, tmp)
	if _, err := g.line(clone...); err != nil {
		return res, err
	}
	g.dir = tmp
	if res.Branch, err = startBranch(g, o.URL); err != nil {
		return res, err
	}
	if err := bootstrap(g, o, &res); err != nil {
		return res, err
	}
	// There is no work tree, so pointing the branch at the remote's tip is all
	// following it takes.
	for _, args := range [][]string{
		{"update-ref", "refs/heads/" + res.Branch, res.Head},
		{"symbolic-ref", "HEAD", "refs/heads/" + res.Branch},
		{"branch", "--quiet", "--set-upstream-to=origin/" + res.Branch, res.Branch},
	} {
		if _, err := g.line(args...); err != nil {
			return res, err
		}
	}
	// A sync must not start in Dir while Init is still filling its index: the
	// lock moves into Dir with the git directory, and a hook's sync skips it.
	lockPath := filepath.Join(tmp, ".git", syncLockName)
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return res, err
	}
	if err := placeFleetFile(g, o.Dir, filepath.Join(tmp, ".git"), res.Head); err != nil {
		return res, err
	}
	if err := RenameRetry(ctx, filepath.Join(tmp, ".git"), filepath.Join(o.Dir, ".git")); err != nil {
		if _, statErr := os.Lstat(filepath.Join(o.Dir, ".git")); statErr == nil {
			// Another init set Dir up first; finish as on any clone.
			return initClone(ctx, o, url, run)
		}
		return res, err
	}
	defer os.Remove(filepath.Join(o.Dir, ".git", syncLockName))
	res.Cloned = true
	g.dir = o.Dir
	if _, err := g.line("read-tree", "HEAD"); err != nil {
		return res, err
	}
	res.Restored, err = restoreMissing(g)
	return res, err
}

func initClone(ctx context.Context, o InitOptions, url string, run Runner) (InitResult, error) {
	var res InitResult
	g := git{ctx: ctx, dir: o.Dir, run: run}
	top, err := g.line("rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return res, err
		}
		return res, fmt.Errorf("%w: %s (%v)", ErrNotClone, o.Dir, err)
	}
	if !SameDir(top, o.Dir) {
		return res, fmt.Errorf("%w: %s is inside the repository at %s", ErrNotClone, o.Dir, top)
	}
	origin, err := g.line("config", "--get", "remote.origin.url")
	if err != nil && ctx.Err() != nil {
		return res, err
	}
	if !sameURL(origin, url) {
		return res, fmt.Errorf("%w: %s follows %s, not %s", ErrOtherRemote, o.Dir, origin, url)
	}
	// A clone with no commit, such as a plain clone of the journal repository made
	// while it was still empty, has nothing to follow yet: it is set up in place,
	// as initNew sets up a new directory, on the branch it is on. One with branches
	// is not, even on a branch with no commit (git checkout --orphan): setting it
	// up would move a branch that may hold commits of its own.
	_, err = g.line("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil && ctx.Err() != nil {
		return res, err
	}
	unborn := err != nil
	if unborn {
		branches, err := g.line("for-each-ref", "--format=%(refname)", "refs/heads/")
		if err != nil {
			return res, err
		}
		unborn = branches == ""
	}
	if !unborn {
		upstream, err := g.line("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			return res, fmt.Errorf("%w: set one with `git push -u origin <branch>` in %s", ErrNoUpstream, o.Dir)
		}
		remote, branch, ok := strings.Cut(upstream, "/")
		if !ok || remote != "origin" {
			return res, fmt.Errorf("%w: upstream %q is not origin/<branch>", ErrNoUpstream, upstream)
		}
		res.Branch = branch
	}
	unlock, gitDir, err := lock(g)
	if err != nil {
		return res, err
	}
	defer unlock()
	// An init stopped after moving its clone's git directory in, and before
	// filling the index, left a clone with no index: every file would look
	// deleted, and untracked, to git. Filling it now finishes that init.
	if _, err := os.Stat(filepath.Join(gitDir, "index")); !unborn && errors.Is(err, os.ErrNotExist) {
		if _, err := g.line("read-tree", "HEAD"); err != nil {
			return res, err
		}
	}
	if _, err := g.line(append(batchSSH(g), "fetch", "--quiet", "--no-tags", "origin")...); err != nil {
		return res, err
	}
	if unborn {
		if res.Branch, err = startBranch(g, o.URL); err != nil {
			return res, err
		}
	}
	if err := bootstrap(g, o, &res); err != nil {
		return res, err
	}
	if unborn {
		for _, args := range [][]string{
			{"update-ref", "refs/heads/" + res.Branch, res.Head},
			{"symbolic-ref", "HEAD", "refs/heads/" + res.Branch},
			{"branch", "--quiet", "--set-upstream-to=origin/" + res.Branch, res.Branch},
			{"read-tree", "HEAD"},
		} {
			if _, err := g.line(args...); err != nil {
				return res, err
			}
		}
		res.Cloned = true
	}
	if err := adoptFleetFile(g, o.Dir, gitDir, res.Head); err != nil {
		return res, err
	}
	res.Restored, err = restoreMissing(g)
	return res, err
}

// adoptFleetFile makes a clone's FleetFile the journal's, file and index entry,
// as placeFleetFile does for a new one. A FleetFile with another salt, edited by
// hand or left by an earlier setup, gives way, and its salt is noted in gitDir as
// placeFleetFile notes it, so that init's own sync files the records this machine
// wrote under it. The file is compared with the journal's as git would store it,
// so that an equal one, CRLF endings and all, is never rewritten under a hook.
// One that differs is replaced before the index entry changes: an init stopped
// between the two then leaves the journal's salt in place, where the other order
// would leave a file that differs from git's copy, which no sync replaces.
func adoptFleetFile(g git, dir, gitDir, tip string) error {
	want, err := g.raw(nil, "cat-file", "blob", tip+":"+FleetFile)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FleetFile)
	have, err := os.ReadFile(path)
	switch {
	case err == nil:
		f, perr := parseFleet(have, path)
		w, _ := parseFleet([]byte(want), FleetFile)
		if perr == nil && f.Salt != w.Salt {
			if err := NotePastSalt(gitDir, f.Salt); err != nil {
				return err
			}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	// "<mode> blob <id>\t<path>" from the tree, "<mode> <id> 0\t<path>" from the index.
	entry, err := g.line("ls-tree", tip, "--", FleetFile)
	if err != nil {
		return err
	}
	mode, rest, _ := strings.Cut(entry, " ")
	id, _, _ := strings.Cut(strings.TrimPrefix(rest, "blob "), "\t")
	same := false
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		got, err := g.line("hash-object", "--", FleetFile)
		if err != nil {
			return err
		}
		same = got == id
	}
	if !same {
		if err := writeFleetFile(g.ctx, gitDir, path, want); err != nil {
			return err
		}
	}
	staged, err := g.line("ls-files", "--stage", "--", FleetFile)
	if err != nil || staged == mode+" "+id+" 0\t"+FleetFile {
		return err
	}
	_, err = g.line("update-index", "--add", "--cacheinfo", mode+","+id+","+FleetFile)
	return err
}

// removeAbandonedClones removes the temporary clones beside dir that an init
// killed before it finished left behind: directories named as MkdirTemp names
// them, holding nothing but a git directory, if that. One younger than staleLock
// may belong to an init still running, and is left alone.
func removeAbandonedClones(dir string) {
	base := filepath.Base(dir) + ".init-"
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), base+"*"))
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || !info.IsDir() || time.Since(info.ModTime()) <= staleLock {
			continue
		}
		if suffix := strings.TrimPrefix(filepath.Base(m), base); suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		entries, err := os.ReadDir(m)
		if err != nil || len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != ".git") {
			continue
		}
		os.RemoveAll(m)
	}
}

// startBranch names the branch the journal is on: the remote's default branch.
// When that does not exist but the remote has exactly one branch, the journal is
// on that one: a server that does not say which branch is its default (git
// before 2.31, or protocol v0) leaves the clone on unnamedDefault, and the first
// machine to start the journal there started main. With several branches and no
// default among them the journal could be on any, so init refuses. A repository
// with no branch at all is empty, and the journal starts the branch the remote
// names as its default, else main, but never this machine's own default, which
// another machine starting the same journal at the same moment may not share.
func startBranch(g git, url string) (string, error) {
	branch, err := g.line("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("gitsync: the clone has no branch: %w", err)
	}
	if _, err := g.line("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		return branch, nil
	}
	out, err := g.line("for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/")
	if err != nil {
		return "", err
	}
	var others []string
	for _, name := range strings.Split(out, "\n") {
		if name != "" && name != "HEAD" {
			others = append(others, name)
		}
	}
	switch {
	case len(others) > 1:
		named := "names no branch"
		if branch != unnamedDefault {
			named = "is " + branch + ", which does not exist"
		}
		return "", fmt.Errorf("%w: %s's default branch %s, but it has %s; make the branch that holds the journal "+
			"its default, then run fleetd init again", ErrNoDefaultBranch, url, named, strings.Join(others, ", "))
	case len(others) == 1:
		branch = others[0]
	case branch == unnamedDefault:
		branch = "main"
	default:
		return branch, nil
	}
	if _, err := g.line("symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", err
	}
	return branch, nil
}

// bootstrap makes sure the remote branch has FleetFile, and sets res.Head and
// res.Salt from it. It moves refs only, never a work tree.
func bootstrap(g git, o InitOptions, res *InitResult) error {
	ssh := batchSSH(g)
	for attempt := 1; ; attempt++ {
		tip, _ := g.line("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+res.Branch)
		var top map[string]string
		if tip != "" {
			var err error
			if top, err = topLevel(g, tip); err != nil {
				return err
			}
			if err := looksLikeJournal(top); err != nil {
				return fmt.Errorf("%w (%s): %v", ErrNotJournal, o.URL, err)
			}
			if _, ok := top[FleetFile]; ok {
				data, err := g.raw(nil, "cat-file", "blob", tip+":"+FleetFile)
				if err != nil {
					return err
				}
				fleet, err := parseFleet([]byte(data), o.URL+"'s "+FleetFile)
				if err != nil {
					return err
				}
				if o.Salt != "" && o.Salt != fleet.Salt {
					if o.Strict {
						return ErrSaltMismatch
					}
					res.SaltDiffers = true
				}
				res.Head, res.Salt = tip, fleet.Salt
				return nil
			}
		}
		if attempt > MaxAttempts {
			return fmt.Errorf("gitsync: %s still has no %s after %d attempts", o.URL, FleetFile, MaxAttempts)
		}
		salt := o.Salt
		if salt == "" {
			for name := range top {
				if strings.HasSuffix(name, ".jsonl") {
					return fmt.Errorf("%w: %s holds %s", ErrNeedSalt, o.URL, name)
				}
			}
			var err error
			if salt, err = randomSalt(); err != nil {
				return err
			}
		}
		commit, err := fleetCommit(g, tip, salt)
		if err != nil {
			return err
		}
		out, err := g.line(append(ssh, "push", "--porcelain", "--no-verify", "origin", commit+":refs/heads/"+res.Branch)...)
		switch {
		case err == nil:
			res.WroteFleetFile, res.Started = true, tip == ""
			if res.Started {
				if err := oneBranch(g, res.Branch); err != nil {
					return err
				}
			}
		case refused(out):
			return fmt.Errorf("%w (it may protect %s from direct pushes; fleetd needs to push to it): %w", ErrRejected, res.Branch, err)
		case !lostRace(out):
			return err
		}
		// Either way the remote has a tip to look at again: this machine's
		// commit, or the one that beat it.
		if _, err := g.line(append(ssh, "fetch", "--quiet", "--no-tags", "origin")...); err != nil {
			return err
		}
	}
}

// oneBranch fails when a journal this machine just started has another branch:
// two machines started it at once on two branches, each with its own salt.
func oneBranch(g git, branch string) error {
	out, err := g.line(append(batchSSH(g), "ls-remote", "--heads", "origin")...)
	if err != nil {
		return err
	}
	var others []string
	for _, l := range strings.Split(out, "\n") {
		if _, ref, ok := strings.Cut(l, "\t"); ok && ref != "refs/heads/"+branch {
			others = append(others, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("gitsync: the journal was started on two branches at once, %s and %s; "+
			"delete the one that is not the repository's default, then run fleetd init again", branch, strings.Join(others, ", "))
	}
	return nil
}

// placeFleetFile writes the journal's FleetFile into dir, so that a record
// appended from then on uses the journal's salt. dir may hold one already, from
// an interrupted init or written by hand: the journal's replaces it, and a salt
// it held that the journal does not use is noted in gitDir, so that the records
// this machine wrote under it are filed under the journal's later.
func placeFleetFile(g git, dir, gitDir, tip string) error {
	want, err := g.raw(nil, "cat-file", "blob", tip+":"+FleetFile)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FleetFile)
	have, err := os.ReadFile(path)
	switch {
	case err == nil && string(have) == want:
		return nil
	case err == nil:
		f, perr := parseFleet(have, path)
		w, _ := parseFleet([]byte(want), FleetFile)
		if perr == nil && f.Salt != w.Salt {
			if err := NotePastSalt(gitDir, f.Salt); err != nil {
				return err
			}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFleetFile(g.ctx, filepath.Dir(dir), path, want)
}

// writeFleetFile puts content at path by renaming in a file written in tmpDir, so
// that a reader never sees half of it.
func writeFleetFile(ctx context.Context, tmpDir, path, content string) error {
	f, err := os.CreateTemp(tmpDir, "."+FleetFile+".*")
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = RenameRetry(ctx, f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// pastSaltsName is the file, in a clone's git directory, that lists the salts
// this machine recorded under that the journal does not use: one a fleetd.json
// in the journal directory held before init replaced it, and one the journal's
// fleetd.json held before it changed. It is never committed.
const pastSaltsName = "fleetd-past-salts"

// PastSalts returns the salts noted in gitDir, oldest first.
func PastSalts(gitDir string) []string {
	return ReadSalts(filepath.Join(gitDir, pastSaltsName))
}

// NotePastSalt adds salt to the salts noted in gitDir.
func NotePastSalt(gitDir, salt string) error {
	return AppendSalt(filepath.Join(gitDir, pastSaltsName), salt)
}

// AppendSalt adds salt to the salts file at path, once. Each salt is a line of
// its own, holding it as a quoted Go string, so that it reads back exactly,
// bytes that are not UTF-8 included. A line is appended in a single write, so
// that processes noting salts at once lose none of them, as they would if each
// rewrote the file. Anything at path but a regular file, such as a symbolic link
// or a FIFO an account able to write there could plant, is refused.
func AppendSalt(path, salt string) error {
	data, err := readRegular(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if slices.Contains(parseSalts(data), salt) {
		return nil
	}
	line := strconv.Quote(salt) + "\n"
	// A write that failed partway, on a full disk say, left its line unfinished;
	// this one starts a line of its own.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		line = "\n" + line
	}
	f, err := journal.OpenRegular(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadSalts returns the salts in the salts file at path, oldest first. A line
// that is not a whole quoted string is skipped, and so is anything at path but a
// regular file.
func ReadSalts(path string) []string {
	data, _ := readRegular(path)
	return parseSalts(data)
}

// readRegular reads the file at path if it is a regular file.
func readRegular(path string) ([]byte, error) {
	f, err := journal.OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func parseSalts(data []byte) []string {
	var salts []string
	for _, line := range strings.Split(string(data), "\n") {
		if salt, err := strconv.Unquote(line); err == nil {
			salts = append(salts, salt)
		}
	}
	return salts
}

// restoreMissing checks out every tracked file the work tree lacks, and returns
// them. A file the work tree has is never overwritten.
func restoreMissing(g git) ([]string, error) {
	out, err := g.raw(nil, "ls-files", "-z", "--deleted")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	if _, err := g.raw(nulList(paths), "checkout-index", "-z", "--stdin"); err != nil {
		return nil, err
	}
	return paths, nil
}

// checkJournalDir refuses a directory init must not take over: anything in it
// but journal files and FleetFile, which init would otherwise leave inside a
// clone it publishes from.
func checkJournalDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var foreign []string
	for _, e := range entries {
		if e.Type().IsRegular() && (strings.HasSuffix(e.Name(), ".jsonl") || e.Name() == FleetFile) {
			continue
		}
		foreign = append(foreign, e.Name())
	}
	if len(foreign) > 0 {
		return fmt.Errorf("%w: %s holds %s; init sets the journal up there, so move it, or give init another --dir",
			ErrDirInUse, dir, strings.Join(foreign, ", "))
	}
	return nil
}

// topLevel lists a commit's top-level entries by name, with their object types.
func topLevel(g git, commit string) (map[string]string, error) {
	listing, err := g.raw(nil, "ls-tree", "-z", commit)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, entry := range strings.Split(listing, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		if fields := strings.Fields(meta); len(fields) == 3 {
			out[name] = fields[1]
		}
	}
	return out, nil
}

// looksLikeJournal refuses a commit whose top level holds anything but host
// journal files, FleetFile, and what GitHub offers to create with a new
// repository; or anything that is not a file.
func looksLikeJournal(top map[string]string) error {
	var foreign []string
	for name, kind := range top {
		lower := strings.ToLower(name)
		switch {
		case kind != "blob":
		case name == FleetFile, hostFile.MatchString(name),
			strings.HasPrefix(lower, "readme"), strings.HasPrefix(lower, "license"),
			lower == ".gitignore", lower == ".gitattributes":
			continue
		}
		foreign = append(foreign, name)
	}
	if len(foreign) > 0 {
		if len(foreign) > 5 {
			foreign = append(foreign[:5], "…")
		}
		return fmt.Errorf("it holds %s", strings.Join(foreign, ", "))
	}
	return nil
}

// fleetCommit builds a commit, without touching a work tree or the index, that
// adds FleetFile to parent, or that holds only FleetFile when parent is empty,
// meaning the repository has no commits yet.
func fleetCommit(g git, parent, salt string) (string, error) {
	content, err := json.MarshalIndent(Fleet{About: fleetAbout, Salt: salt}, "", "  ")
	if err != nil {
		return "", err
	}
	blob, err := g.raw(append(content, '\n'), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	var entries []string
	if parent != "" {
		listing, err := g.raw(nil, "ls-tree", "-z", parent)
		if err != nil {
			return "", err
		}
		for _, entry := range strings.Split(listing, "\x00") {
			if _, path, ok := strings.Cut(entry, "\t"); ok && path != FleetFile {
				entries = append(entries, entry)
			}
		}
	}
	entries = append(entries, "100644 blob "+strings.TrimSpace(blob)+"\t"+FleetFile)
	tree, err := g.raw([]byte(strings.Join(entries, "\x00")+"\x00"), "mktree", "-z")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", "journal: record the fleet's salt"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return g.line(args...)
}

// sameURL reports whether two remote URLs name the same repository: the same
// directory for a local path, otherwise the same text, ignoring a trailing slash
// or .git and letter case.
func sameURL(a, b string) bool {
	if ia, err := os.Stat(a); err == nil && ia.IsDir() {
		return SameDir(a, b)
	}
	norm := func(s string) string {
		return strings.ToLower(strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(s), "/"), ".git"))
	}
	return norm(a) == norm(b)
}

func randomSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RenameRetry renames, retrying for a few seconds while Windows refuses because
// another process, such as a virus scanner or an indexer, has the file open. It
// stops retrying once ctx ends.
func RenameRetry(ctx context.Context, from, to string) error {
	var err error
	for delay := 50 * time.Millisecond; delay < 5*time.Second; delay *= 2 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if _, statErr := os.Stat(from); statErr != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
	return err
}
