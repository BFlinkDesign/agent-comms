// Package gitsync publishes one machine's journal file through git and brings in
// every other machine's.
//
// The journal directory is the root of a clone that holds one file per host, and
// fleetd owns that clone. Sync never rebases, merges or stashes, and never writes
// this host's file: `fleetd record` may be appending to it at the same moment. It
// builds this host's commit with git plumbing directly on top of the remote tip,
// from a snapshot of the file cut at its last complete line, pushes exactly that
// commit, and only then brings the other hosts' files into the working tree. A
// file with changes that are not on the remote, such as another identity's
// unpublished records on this machine, is left as it is and reported. Only
// working-tree changes count: the clone is fleetd's, so an edit staged with
// `git add` and not changed since is reset to the remote's version.
//
// Because each host owns one file, the only way two hosts can collide is by
// deriving the same host id. That is detected by content rather than by reading
// git's messages: the remote copy of this host's file must be a prefix of the
// local one.
package gitsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxAttempts bounds how many times a push rejected by another machine's
// concurrent push is retried.
const MaxAttempts = 3

// waitDelay bounds how long a git command may outlive its context, for example
// when a child such as ssh keeps its output pipes open.
const waitDelay = 2 * time.Second

// staleLock is how old a sync lock must be before another sync may break it.
const staleLock = 10 * time.Minute

var (
	// ErrNotClone means the journal directory is not the root of a git clone.
	ErrNotClone = errors.New("gitsync: journal directory is not the root of a git clone")
	// ErrNoUpstream means the current branch tracks no remote branch.
	ErrNoUpstream = errors.New("gitsync: current branch has no upstream")
	// ErrSameFile means the remote copy of this host's file holds records this
	// machine never wrote: another machine derives the same host id.
	ErrSameFile = errors.New("gitsync: another machine wrote this host's journal file")
	// ErrLocalCommits means the clone's branch has commits that are not on the
	// remote: made by hand, or left behind when the remote was rewritten. fleetd
	// never makes such commits and does not discard them.
	ErrLocalCommits = errors.New("gitsync: the journal clone has commits that are not on the remote")
	// ErrBusy means another sync of the same clone is running.
	ErrBusy = errors.New("gitsync: another sync of this journal is running")
)

// gitConfig is the configuration every git command runs with. core.fsmonitor
// is emptied rather than set to false: git before 2.36 reads it only as a hook's
// path, so "false" would name a hook to run, while every version reads an empty
// value as no fsmonitor at all.
var gitConfig = []string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.fsmonitor="}

// Runner runs git with args in dir, feeding it stdin, and returns its standard
// output exactly as written.
type Runner func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error)

// Git runs the git binary on PATH. Nothing it runs may wait for a person:
// terminal and credential prompts are off, hooks do not run, and commits are
// never signed, since a signer can prompt. Nothing it runs may outlive it
// either: core.fsmonitor is off, so git starts no fsmonitor daemon. When the context ends, git and every
// process it started are killed: a process group on Unix, a job object on
// Windows. A child that still holds git's output open is cut off after a short
// delay rather than holding the sync open.
func Git(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
	full := append(slices.Clone(gitConfig), args...)
	var stdout, stderr bytes.Buffer
	// A fresh command each time runTree asks, with fresh input and empty output:
	// on Windows git may have to be started a second time.
	newCmd := func() *exec.Cmd {
		stdout.Reset()
		stderr.Reset()
		cmd := exec.CommandContext(ctx, "git", full...)
		cmd.Dir = dir
		cmd.WaitDelay = waitDelay
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_LITERAL_PATHSPECS=1")
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		return cmd
	}
	if err := runTree(newCmd); err != nil {
		if ctx.Err() != nil {
			return stdout.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Options says what to publish.
type Options struct {
	// Dir is the journal directory: the root of a clone of the journal repository.
	Dir string
	// File is this host's journal file, directly inside Dir.
	File string
	// Message is the commit message for this host's new records.
	Message string
	// Run runs git; nil means Git.
	Run Runner
}

// Result says what a sync did.
type Result struct {
	// Published is the number of this host's records newly on the remote.
	Published int `json:"published"`
	// Received is the number of commits from other machines brought in.
	Received int `json:"received"`
	// Head is the remote commit the clone is at afterwards.
	Head string `json:"head"`
	// Attempts is how many push attempts it took.
	Attempts int `json:"attempts"`
	// Kept lists files the remote changed that were left as they are, because
	// this clone has changes to them that are not on the remote.
	Kept []string `json:"kept,omitempty"`
}

type git struct {
	ctx context.Context
	dir string
	run Runner
}

// raw returns git's output untouched; line returns it with surrounding space trimmed.
func (g git) raw(stdin []byte, args ...string) (string, error) {
	return g.run(g.ctx, g.dir, stdin, args...)
}

func (g git) line(args ...string) (string, error) {
	out, err := g.raw(nil, args...)
	return strings.TrimSpace(out), err
}

// Sync publishes this host's complete records and brings in every other host's.
func Sync(ctx context.Context, o Options) (Result, error) {
	g := git{ctx: ctx, dir: o.Dir, run: o.Run}
	if g.run == nil {
		g.run = Git
	}
	var res Result

	top, err := g.line("rev-parse", "--show-toplevel")
	if err != nil {
		return res, fmt.Errorf("%w: %s (%v)", ErrNotClone, o.Dir, err)
	}
	if !sameDir(top, o.Dir) {
		return res, fmt.Errorf("%w: %s is inside the repository at %s; clone the journal repository into a directory of its own",
			ErrNotClone, o.Dir, top)
	}
	if !sameDir(filepath.Dir(o.File), o.Dir) {
		return res, fmt.Errorf("gitsync: %s is not directly inside %s", o.File, o.Dir)
	}
	own := filepath.Base(o.File)

	unlock, err := lock(g)
	if err != nil {
		return res, err
	}
	defer unlock()

	upstream, err := g.line("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return res, fmt.Errorf("%w: set one with `git push -u origin <branch>` in %s", ErrNoUpstream, o.Dir)
	}
	remote, branch, ok := strings.Cut(upstream, "/")
	if !ok {
		return res, fmt.Errorf("%w: upstream %q is not remote/branch", ErrNoUpstream, upstream)
	}
	localRef, err := g.line("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return res, fmt.Errorf("gitsync: HEAD is detached in %s; check out %s", o.Dir, branch)
	}
	local, err := g.line("rev-parse", "--verify", "HEAD")
	if err != nil {
		return res, err
	}

	ssh := batchSSH(g)
	var tip string
	for res.Attempts = 1; ; res.Attempts++ {
		if _, err := g.line(append(ssh, "fetch", "--quiet", "--no-tags", remote)...); err != nil {
			return res, err
		}
		remoteTip, err := g.line("rev-parse", "--verify", "refs/remotes/"+upstream)
		if err != nil {
			return res, err
		}
		if _, err := g.line("merge-base", "--is-ancestor", local, remoteTip); err != nil {
			return res, fmt.Errorf("%w: %s. If they are not wanted, or the remote was rewritten, "+
				"`git -C %s reset --soft '@{upstream}'` makes the clone follow the remote again and keeps "+
				"this machine's unpublished records for the next sync", ErrLocalCommits, o.Dir, o.Dir)
		}
		commit, published, err := snapshotCommit(g, remoteTip, own, o.File, o.Message)
		if err != nil {
			return res, err
		}
		if commit == "" {
			tip = remoteTip
			break
		}
		out, err := g.line(append(ssh, "push", "--porcelain", "--no-verify", remote, commit+":refs/heads/"+branch)...)
		if err == nil {
			tip, res.Published = commit, published
			break
		}
		// Only a push that lost a race to another machine is retried; anything
		// else, such as a refused credential, is reported as it is.
		if !strings.Contains(out, "[rejected]") || res.Attempts >= MaxAttempts {
			return res, err
		}
	}

	received, err := g.line("rev-list", "--count", local+".."+tip)
	if err != nil {
		return res, err
	}
	res.Received, _ = strconv.Atoi(received)
	if res.Published > 0 {
		res.Received-- // this host's own commit
	}
	if res.Kept, err = bringIn(g, localRef, local, tip, own); err != nil {
		return res, err
	}
	res.Head = tip
	return res, nil
}

// batchSSH returns the arguments that keep ssh from waiting for a person, or none
// when the user has chosen an ssh command of their own (a deploy key, plink): that
// choice is theirs, and git would otherwise let this one override it.
func batchSSH(g git) []string {
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return nil
	}
	if configured, _ := g.line("config", "--get", "core.sshCommand"); configured != "" {
		return nil
	}
	return []string{"-c", "core.sshCommand=ssh -o BatchMode=yes"}
}

// snapshotCommit builds, without touching the working tree or the index, a commit
// on top of remoteTip whose only change is this host's file as of its last
// complete line. It returns "" when the remote already has every complete record.
func snapshotCommit(g git, remoteTip, own, file, message string) (string, int, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	// A record still being appended ends without a newline; it waits for the
	// next sync rather than being published torn. Line endings are published as
	// LF: git for Windows checks this file out with CRLF by default, and a JSON
	// record never contains a raw carriage return, so dropping it loses nothing.
	complete := bytes.ReplaceAll(data[:bytes.LastIndexByte(data, '\n')+1], []byte("\r\n"), []byte("\n"))

	var published []byte
	if blob, ok, err := blobAt(g, remoteTip, own); err != nil {
		return "", 0, err
	} else if ok {
		out, err := g.raw(nil, "cat-file", "blob", blob)
		if err != nil {
			return "", 0, err
		}
		published = []byte(out)
	}
	if !bytes.HasPrefix(complete, published) {
		return "", 0, fmt.Errorf("%w: the remote %s holds records this machine never wrote; give each machine a distinct identity (see `fleetd host`)",
			ErrSameFile, own)
	}
	fresh := bytes.Count(complete, []byte{'\n'}) - bytes.Count(published, []byte{'\n'})
	if fresh == 0 {
		return "", 0, nil
	}

	blob, err := g.raw(complete, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", 0, err
	}
	// The new tree is the remote tip's top-level tree with this host's entry
	// replaced, assembled with ls-tree and mktree, so no index is involved.
	listing, err := g.raw(nil, "ls-tree", "-z", remoteTip)
	if err != nil {
		return "", 0, err
	}
	var entries []string
	for _, entry := range strings.Split(listing, "\x00") {
		if _, path, ok := strings.Cut(entry, "\t"); ok && path != own {
			entries = append(entries, entry)
		}
	}
	entries = append(entries, "100644 blob "+strings.TrimSpace(blob)+"\t"+own)
	tree, err := g.raw([]byte(strings.Join(entries, "\x00")+"\x00"), "mktree", "-z")
	if err != nil {
		return "", 0, err
	}
	commit, err := g.line("commit-tree", strings.TrimSpace(tree), "-p", remoteTip, "-m", message)
	if err != nil {
		return "", 0, err
	}
	return commit, fresh, nil
}

// blobAt returns the blob id of a top-level file in a commit, if it has one.
func blobAt(g git, commit, name string) (string, bool, error) {
	entry, err := g.line("ls-tree", commit, "--", name)
	if err != nil || entry == "" {
		return "", false, err
	}
	fields := strings.Fields(entry)
	if len(fields) < 3 || fields[1] != "blob" {
		return "", false, fmt.Errorf("gitsync: %s in %s is not a file", name, commit)
	}
	return fields[2], true, nil
}

// bringIn points the local branch at tip and brings every other file the index
// does not already hold at tip's version into the working tree. This host's file
// is never written. Neither is a file with changes the remote does not have,
// such as another identity's records on this machine or an edit made by hand:
// it is returned, left as it is. Only the working tree is compared with the
// index, so an edit staged with `git add` and not changed since is reset to tip,
// and such a new file that tip lacks is removed; git keeps their content until it
// prunes unreachable objects.
func bringIn(g git, localRef, local, tip, own string) ([]string, error) {
	if _, err := g.line("update-ref", "-m", "fleetd sync", localRef, tip, local); err != nil {
		return nil, err
	}
	// Comparing the index, rather than the old commit, with tip also repairs
	// files a sync interrupted after this point left behind.
	diff, err := g.raw(nil, "diff-index", "--cached", "-z", "--name-status", "--no-renames", tip)
	if err != nil {
		return nil, err
	}
	var update, remove, paths []string
	fields := strings.Split(diff, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i], fields[i+1]
		if path == own {
			continue
		}
		paths = append(paths, path)
		// In the index but not at tip: the remote deleted it. A clean one goes,
		// even if it was staged here by hand, since the clone is fleetd's; git
		// still holds its content.
		if status == "A" {
			remove = append(remove, path)
		} else {
			update = append(update, path)
		}
	}
	var kept []string
	if len(paths) > 0 {
		changed, err := locallyChanged(g, paths)
		if err != nil {
			return nil, err
		}
		keep := func(list []string) []string {
			var out []string
			for _, path := range list {
				if changed[path] {
					kept = append(kept, path)
				} else {
					out = append(out, path)
				}
			}
			return out
		}
		update, remove = keep(update), keep(remove)
		// Writing a path the remote turned from a directory into a file, or back,
		// replaces everything under it or above it; a kept file there would go too.
		var safe []string
		for _, path := range update {
			if nests(path, kept) {
				kept = append(kept, path)
			} else {
				safe = append(safe, path)
			}
		}
		update = safe
	}
	// git does both the removing and the writing, never Go's os package: git will
	// not follow a symbolic link out of the clone, so a remote commit that turns a
	// directory into a link cannot make a sync touch anything outside it.
	// Removals go first, so a directory the remote replaced with a file is gone
	// before the file is written.
	if len(remove) > 0 {
		if _, err := g.raw(nulList(remove), "checkout", "--no-overlay", tip, "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return nil, err
		}
	}
	if len(update) > 0 {
		if _, err := g.raw(nulList(update), "checkout", tip, "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return nil, err
		}
	}
	// Keep this host's index entry equal to the remote's, so `git status` shows
	// exactly the records not yet published.
	if blob, ok, err := blobAt(g, tip, own); err != nil {
		return nil, err
	} else if ok {
		if _, err := g.line("update-index", "--add", "--cacheinfo", "100644,"+blob+","+own); err != nil {
			return nil, err
		}
	}
	return kept, nil
}

// locallyChanged reports which paths hold content the index does not: a file
// modified in the working tree, or one git does not track.
func locallyChanged(g git, paths []string) (map[string]bool, error) {
	out, err := g.raw(nil, append([]string{"status", "--porcelain=v1", "-z", "--no-renames",
		"--untracked-files=all", "--ignored=matching", "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, entry := range strings.Split(out, "\x00") {
		// "XY path": Y compares the working tree with the index. A file deleted
		// here has nothing to lose.
		if len(entry) > 3 && entry[1] != ' ' && entry[1] != 'D' {
			changed[entry[3:]] = true
		}
	}
	return changed, nil
}

// nests reports whether path is a directory above one of others, or inside one.
func nests(path string, others []string) bool {
	for _, other := range others {
		if strings.HasPrefix(other, path+"/") || strings.HasPrefix(path, other+"/") {
			return true
		}
	}
	return false
}

func nulList(paths []string) []byte {
	return []byte(strings.Join(paths, "\x00") + "\x00")
}

// lock takes a lock file in the clone's git directory so two syncs of the same
// clone never interleave. A lock older than staleLock is taken to be abandoned.
func lock(g git) (func(), error) {
	gitDir, err := g.line("rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(gitDir, "fleetd-sync.lock")
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) < staleLock {
			return nil, fmt.Errorf("%w: %s exists", ErrBusy, path)
		}
		os.Remove(path)
	}
	return nil, fmt.Errorf("%w: %s exists", ErrBusy, path)
}

// sameDir reports whether two paths name the same directory, after resolving
// symbolic links; on Windows letter case does not matter.
func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	ra, errA = filepath.Abs(ra)
	rb, errB = filepath.Abs(rb)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ra, rb)
	}
	return ra == rb
}
