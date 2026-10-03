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
// `git add` and not changed since is reset to the remote's version, and a file
// that holds only the start of git's copy, such as one restored from an older
// backup, is brought up to date.
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

// packLimit is how many loose objects a journal clone may hold before a sync
// packs it: every sync adds a few, and git's automatic gc is off inside one.
var packLimit = 1000

// staleLock is how old a sync lock must be before another sync may break it.
const staleLock = 10 * time.Minute

var (
	// ErrNotClone means the journal directory is not the root of a git clone.
	ErrNotClone = errors.New("gitsync: journal directory is not the root of a git clone")
	// ErrNoUpstream means the clone's HEAD is not on a branch that follows one of
	// origin's, so a sync has nowhere to publish.
	ErrNoUpstream = errors.New("gitsync: the clone is not on the journal's branch")
	// ErrSameFile means the remote copy of this host's file holds records this
	// machine never wrote: another machine derives the same host id.
	ErrSameFile = errors.New("gitsync: another machine wrote this host's journal file")
	// ErrLocalCommits means the clone's branch has commits that are not on the
	// remote: made by hand, or left behind when the remote was rewritten. fleetd
	// never makes such commits and does not discard them.
	ErrLocalCommits = errors.New("gitsync: the journal clone has commits that are not on the remote")
	// ErrBusy means another sync of the same clone is running.
	ErrBusy = errors.New("gitsync: another sync of this journal is running")
	// ErrRejected means the remote declined this machine's push: a hook, branch
	// protection or a ruleset. No later push gets past it until a person changes
	// the remote.
	ErrRejected = errors.New("gitsync: the remote refused this machine's push")
)

// gitConfig is the configuration every git command runs with. core.fsmonitor
// is emptied rather than set to false: git before 2.36 reads it only as a hook's
// path, so "false" would name a hook to run, while every version reads an empty
// value as no fsmonitor at all. Automatic gc and maintenance are off: a fetch
// would otherwise start them inside the sync's deadline, and one killed partway
// leaves lock files that fail every later sync until someone deletes them.
// diff.autoRefreshIndex is on, as by default: with it off, `git diff` lists a file
// whose content matches git's copy but whose index entry is out of date, and
// fleetd would take it for one with records git does not have.
var gitConfig = []string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.fsmonitor=", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
	"-c", "diff.autoRefreshIndex=true"}

// repositoryVariables are the variables git reads to find a repository, its
// index or its objects, and a git command's own -c settings, as
// `git rev-parse --local-env-vars` lists them. git exports some of them to its
// hooks, so fleetd run from a hook, or from anything a hook starts, would
// otherwise point every command below at the hook's repository: publishing the
// journal into it, or refusing to sync at all. GIT_CONFIG_COUNT is not among
// them, as git itself keeps it for a command it runs in another repository:
// git never sets it, so it is configuration the environment sets on purpose,
// such as a URL rewrite, a trusted directory or an ssh command.
var repositoryVariables = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}

// commitDates are dropped too: git exports the dates of the commit it is making
// to its hooks, and an amend carries the original commit's, so a sync run from a
// hook would otherwise date its journal commit years back.
var commitDates = []string{"GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE"}

// identity is who fleetd's journal commits are by. It is not the person's: a
// PC's git identity may be missing, which fails commit-tree, or a private
// address GitHub refuses to publish (push declined, GH007), and it does not
// belong in a journal every machine reads. The host is in each commit's message
// and file already.
var identity = []string{"GIT_AUTHOR_NAME=fleetd", "GIT_AUTHOR_EMAIL=fleetd@fleetd.invalid",
	"GIT_COMMITTER_NAME=fleetd", "GIT_COMMITTER_EMAIL=fleetd@fleetd.invalid"}

// gitEnv is the environment every git command runs with: this process's, less
// the variables that would point git at another repository or name another
// author, plus fleetd's own settings. Names are compared ignoring case, as
// Windows does.
func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(repositoryVariables, func(v string) bool { return strings.EqualFold(v, name) }) ||
			slices.ContainsFunc(commitDates, func(v string) bool { return strings.EqualFold(v, name) }) ||
			slices.ContainsFunc(identity, func(v string) bool { return strings.EqualFold(v[:strings.IndexByte(v, '=')], name) }) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_LITERAL_PATHSPECS=1")
	return append(env, identity...)
}

// Runner runs git with args in dir, feeding it stdin, and returns its standard
// output exactly as written.
type Runner func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error)

// runGitTree is runTree, as a variable so a test can stand in for it.
var runGitTree = runTree

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
		cmd.Env = gitEnv()
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		return cmd
	}
	if err := runGitTree(newCmd); err != nil {
		name := strings.Join(args, " ")
		ctxErr := ctx.Err()
		switch {
		case ctxErr == nil:
			return stdout.String(), fmt.Errorf("git %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
		case errors.Is(err, ctxErr), isExit(err):
			// Killing git is how the context's end stops it, and a killed git
			// exits unsuccessfully: that is all the timeout, not a second problem.
			return stdout.String(), fmt.Errorf("git %s: %w", name, ctxErr)
		default:
			// The context ended and something more went wrong, such as a git
			// that could not be killed and was left behind: say both.
			return stdout.String(), fmt.Errorf("git %s: %w: %w", name, ctxErr, err)
		}
	}
	return stdout.String(), nil
}

// isExit reports whether err is git exiting unsuccessfully, as opposed to git
// not starting or not being ended.
func isExit(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}

// Options says what to publish.
type Options struct {
	// Dir is the journal directory: the root of a clone of the journal repository.
	Dir string
	// File is this host's journal file, directly inside Dir.
	File string
	// Message is the commit message for this host's new records.
	Message string
	// Prepare, when set, runs once the sync holds its lock and the clone has an
	// index, before it reads anything else, given the sync's context, which bounds
	// it, and the clone's git directory. Work on this machine's journal files that
	// must not interleave with another sync goes there.
	Prepare func(ctx context.Context, gitDir string)
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
	// Cleared lists git lock files, relative to the git directory, that were
	// older than staleLock and removed: left by a git command that was killed.
	Cleared []string `json:"cleared,omitempty"`
	// Packed is set when the sync packed the clone's loose objects.
	Packed bool `json:"packed,omitempty"`
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
		if ctx.Err() != nil {
			return res, err
		}
		return res, fmt.Errorf("%w: %s (%v)", ErrNotClone, o.Dir, err)
	}
	if !SameDir(top, o.Dir) {
		return res, fmt.Errorf("%w: %s is inside the repository at %s; clone the journal repository into a directory of its own",
			ErrNotClone, o.Dir, top)
	}
	if !SameDir(filepath.Dir(o.File), o.Dir) {
		return res, fmt.Errorf("gitsync: %s is not directly inside %s", o.File, o.Dir)
	}
	own := filepath.Base(o.File)

	unlock, gitDir, err := lock(g)
	if err != nil {
		return res, err
	}
	defer unlock()
	res.Cleared = clearStaleLocks(gitDir)
	// An init stopped between moving its clone's git directory in and filling
	// the index leaves a clone without one, where every file would look deleted
	// and untracked. A sync that comes first fills it, as init would. A
	// repository with no commit has no index either, and nothing to fill it from.
	if _, err := os.Stat(filepath.Join(gitDir, "index")); errors.Is(err, os.ErrNotExist) {
		if _, err := g.line("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
			if _, err := g.line("read-tree", "HEAD"); err != nil {
				return res, err
			}
		}
	}
	if o.Prepare != nil {
		o.Prepare(ctx, gitDir)
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("gitsync: preparing the sync: %w", err)
		}
	}

	// Only a branch of origin's: fleetd's clone follows the journal there, and a
	// branch of another remote, or of the clone, would take this machine's records
	// somewhere the fleet never looks. Full names, so that a branch here named
	// origin/main never passes for origin's.
	upstream, err := g.line("rev-parse", "--symbolic-full-name", "@{u}")
	if err != nil && ctx.Err() == nil {
		// A branch of origin's that an earlier sync found gone may be back, pushed
		// again after a mistaken deletion, and only a fetch brings it back here.
		again, ferr := refetchGone(g)
		if ferr != nil {
			return res, ferr
		}
		if again {
			upstream, err = g.line("rev-parse", "--symbolic-full-name", "@{u}")
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return res, err
		}
		return res, noUpstream(g)
	}
	branch, ok := strings.CutPrefix(upstream, "refs/remotes/origin/")
	if !ok {
		return res, noUpstream(g)
	}
	// Nor another of origin's branches than the one init set the clone up on, as
	// after a person's git switch, or a repair cut short: init puts it back.
	recorded, err := recordedBranch(gitDir)
	if err != nil {
		return res, err
	}
	if recorded != "" && recorded != branch {
		return res, fmt.Errorf("%w: it follows origin/%s, while the journal is on %s, the branch init set it up on: "+
			"`fleetd init --dir \"%s\" <journal URL>` puts it back there, leaving its files as they are, or says what "+
			"stops it; if the journal has moved, `fleetd init --dir \"%s\" --branch <branch> <journal URL>` puts it "+
			"on <branch>", ErrNoUpstream, branch, recorded, g.dir, g.dir)
	}
	remote := "origin"
	upstream = remote + "/" + branch
	localRef, err := g.line("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return res, err
		}
		return res, noUpstream(g)
	}
	// A branch with no commit yet, as a plain clone of the repository made while
	// it was empty has once it fetches, follows a branch that resolves.
	local, err := g.line("rev-parse", "--verify", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return res, err
		}
		return res, noUpstream(g)
	}

	ssh := batchSSH(g)
	var tip string
	for res.Attempts = 1; ; res.Attempts++ {
		if err := fetchOrigin(g); err != nil {
			return res, err
		}
		remoteTip, err := g.line("rev-parse", "--verify", "refs/remotes/"+upstream)
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			return res, goneError(g, localRef, branch)
		}
		if _, err := g.line("merge-base", "--is-ancestor", local, remoteTip); err != nil {
			if ctx.Err() != nil {
				return res, err
			}
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
		// Onto the tip this sync fetched and nothing else: a branch the remote has
		// renamed or deleted since, or moved on, refuses it, as a race lost.
		out, err := g.line(append(ssh, "push", "--porcelain", "--no-verify", "--force-with-lease=refs/heads/"+branch+":"+remoteTip,
			remote, commit+":refs/heads/"+branch)...)
		if err == nil {
			tip, res.Published = commit, published
			break
		}
		// Only a push that lost a race to another machine is retried; anything
		// else, such as a refused credential, is reported as it is.
		if refused(out) {
			return res, fmt.Errorf("%w (it may protect %s from direct pushes; fleetd needs to push to it): %w",
				ErrRejected, branch, err)
		}
		if !lostRace(out) || res.Attempts >= MaxAttempts {
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
	res.Packed = pack(g)
	return res, nil
}

// pack packs the clone once it holds packLimit loose objects. Every sync adds a
// few, and every fetch of fewer than a hundred objects adds them loose, so a
// clone nothing packs grows without end. git's automatic gc is off for every
// command above, so that none starts inside a fetch; this runs gc at the end
// instead, once the sync has done its work, with what is left of its deadline.
// It is best effort: a clone that is not packed still syncs.
//
// The gc packs objects and does nothing else. Packing refs, expiring reflogs and
// writing the commit graph each take a lock, and a gc the deadline kills leaves
// it behind, which fails every later sync until it is staleLock old. Packing
// objects takes none.
func pack(g git) bool {
	out, err := g.line("count-objects", "-v")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "count: "); ok {
			if loose, _ := strconv.Atoi(strings.TrimSpace(v)); loose < packLimit {
				return false
			}
			_, err := g.line("-c", "gc.packRefs=false", "-c", "gc.reflogExpire=never",
				"-c", "gc.reflogExpireUnreachable=never", "-c", "gc.writeCommitGraph=false", "gc", "--quiet")
			return err == nil
		}
	}
	return false
}

// lostRace reports whether push's porcelain output says the push lost a race
// to another machine: "[rejected]" when the remote had already moved, or
// "[remote rejected]" for a ref the remote could not update because another push
// was updating it, as GitHub reports a race it catches late. A remote rejection
// for any other reason, such as a declined hook or a protected branch, is not a
// race, and retrying it would only repeat it.
func lostRace(out string) bool {
	if strings.Contains(out, "[rejected]") {
		return true
	}
	if !strings.Contains(out, "[remote rejected]") {
		return false
	}
	for _, reason := range []string{"cannot lock ref", "failed to update ref", "incorrect old value"} {
		if strings.Contains(out, reason) {
			return true
		}
	}
	return false
}

// refused reports whether the remote declined a push for good: a hook, branch
// protection or a ruleset said no ("pre-receive hook declined", "protected branch
// hook declined", "push declined due to ..."). A race, a credential that cannot
// push, or a failure on the remote's side, such as its storage, is not refused:
// the next push can get past it.
func refused(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		_, reason, ok := strings.Cut(line, "\t[remote rejected] (")
		if ok && strings.HasPrefix(line, "!") && strings.Contains(reason, "declined") {
			return true
		}
	}
	return false
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
		dir, err := filepath.Abs(filepath.Dir(file))
		if err != nil {
			dir = filepath.Dir(file)
		}
		return "", 0, fmt.Errorf("%w: the remote %s holds records this machine's copy lacks. If this machine's journal "+
			"directory was set up again, or restored from an older copy, `fleetd init --reclaim --dir \"%s\" <journal URL>` "+
			"puts them back and publishes this machine's newer records after them; otherwise another machine has this "+
			"machine's id, and each needs a distinct one (see `fleetd host`)", ErrSameFile, own, dir)
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
		behind, err := behindIndex(g, mapKeys(changed))
		if err != nil {
			return nil, err
		}
		for path := range behind {
			delete(changed, path)
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
	// A file that holds only the start of git's own copy, such as one restored
	// from an older backup, is brought up to date even when the remote did not
	// change it: it has no change of its own to keep.
	modified, err := g.raw(nil, "diff", "--name-only", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, path := range strings.Split(modified, "\x00") {
		if path != "" && path != own {
			candidates = append(candidates, path)
		}
	}
	if stale, err := behindIndex(g, candidates); err != nil {
		return nil, err
	} else if len(stale) > 0 {
		if _, err := g.raw(nulList(mapKeys(stale)), "checkout-index", "-f", "-z", "--stdin"); err != nil {
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

// behindIndex returns, of paths, the regular files whose content is a strict
// start of the index's copy, line endings aside: files with nothing of their own
// that git's copy lacks. A file that cannot be read, or is not a regular file, is
// not among them. Nor is one more than twice the size of the index's copy: a
// strict start of it is shorter, and CRLF line endings at most double that, so
// such a file is never read.
func behindIndex(g git, paths []string) (map[string]bool, error) {
	behind := map[string]bool{}
	for _, path := range paths {
		size, err := g.line("cat-file", "-s", ":"+path)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			continue
		}
		data, err := readRegular(filepath.Join(g.dir, path), 2*n)
		if err != nil {
			continue
		}
		indexed, err := g.raw(nil, "cat-file", "blob", ":"+path)
		if err != nil {
			continue
		}
		have := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		if len(have) < len(indexed) && strings.HasPrefix(indexed, string(have)) {
			behind[path] = true
		}
	}
	return behind, nil
}

// noUpstream is the error for a clone whose HEAD is not on a branch that follows
// one of origin's: detached, on a branch with no commit, or on one that follows
// nothing, a branch of the clone or another remote's. fleetd init puts such a
// clone back on the journal's branch, moving refs only and never a file, so
// running it is the advice: the git commands that move a branch would refuse, or
// would overwrite this machine's records, when the work tree has records the
// branch's files lack. A current branch that follows one of origin's the clone no
// longer has is told so instead: the remote may have deleted or renamed it, and
// init would not know where the journal went.
func noUpstream(g git) error {
	head, err := g.line("symbolic-ref", "--quiet", "HEAD")
	if err != nil && g.ctx.Err() != nil {
		return err
	}
	if err == nil {
		theirs, err := goneUpstream(g, head)
		if err != nil {
			return err
		}
		if theirs != "" {
			return goneError(g, head, theirs)
		}
	}
	return fmt.Errorf("%w: `fleetd init --dir \"%s\" <journal URL>` puts it back there, leaving its files as they are, "+
		"or says what stops it", ErrNoUpstream, g.dir)
}

// goneUpstream names the branch of origin's that the branch head follows when
// this clone no longer has it, else "". The branch is looked up by its full name,
// never as a pattern, which would take zz/a for zz.
func goneUpstream(g git, head string) (string, error) {
	ups, err := upstreams(g)
	if err != nil {
		return "", err
	}
	theirs, ok := strings.CutPrefix(ups[strings.TrimPrefix(head, "refs/heads/")], "refs/remotes/origin/")
	if !ok {
		return "", nil
	}
	if _, err := g.line("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+theirs); err == nil {
		return "", nil
	}
	return theirs, g.ctx.Err()
}

// goneError says what to do about a branch that follows one of origin's the
// clone no longer has. Branch names come from the remote, so none is put into a
// command a person might paste. Only a person knows where the journal went after
// a rename, or a deletion on purpose, so the way on is init told the branch.
func goneError(g git, head, theirs string) error {
	return fmt.Errorf("%w: %s follows origin/%s, which this clone no longer has, as when the remote deleted or renamed "+
		"it. If it was renamed, or deleted on purpose, `fleetd init --dir \"%s\" --branch <branch> <journal URL>` "+
		"puts the clone on <branch>, the one the journal is on now; if it was deleted by mistake, push it back from "+
		"the machine that synced last, with `git push origin HEAD:<branch>` in its journal directory and %s for "+
		"<branch>, and every machine's next sync takes it up again", ErrNoUpstream, strings.TrimPrefix(head, "refs/heads/"),
		theirs, g.dir, theirs)
}

// branchName is the file, in a clone's git directory, that records the journal's
// branch: the one init set the clone up on, which it puts the clone back on, and
// the one every sync checks the clone follows. It is never committed.
const branchName = "fleetd-branch"

// RecordedBranch is the journal's branch as init last set up the clone whose git
// directory is gitDir, or "" when init never recorded one.
func RecordedBranch(gitDir string) (string, error) {
	return recordedBranch(gitDir)
}

// recordedBranch is the journal's branch as init last set up the clone whose git
// directory is gitDir, or "" when init never recorded one, as for a clone set up
// before it did. A record that cannot be read, a directory or a FIFO put there,
// say, is an error, never taken for none.
func recordedBranch(gitDir string) (string, error) {
	path := filepath.Join(gitDir, branchName)
	data, err := readRegular(path, 4096)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("gitsync: %s, init's record of the journal's branch, cannot be read (%v); delete it, then "+
			"run fleetd init again", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// refetchGone fetches when HEAD's branch follows one of origin's that this clone
// no longer has, and reports whether it did: a branch pruned once is never
// looked at again otherwise, even after a person pushes it back.
func refetchGone(g git) (bool, error) {
	head, err := g.line("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return false, g.ctx.Err()
	}
	theirs, err := goneUpstream(g, head)
	if err != nil || theirs == "" {
		return false, err
	}
	return true, fetchOrigin(g)
}

// fetchOrigin brings in every branch of origin's, as a remote-tracking branch of
// the same name, whatever the clone's own refspec says: one made with
// --single-branch names one branch, and a fetch of it fails outright once the
// remote has deleted or renamed that branch. It prunes, so that a branch the
// remote deleted or renamed goes from the clone too, never to be pushed back; and
// since it names its refspec, git leaves the clone's tags alone even where a
// person's configuration says fetch.pruneTags.
func fetchOrigin(g git) error {
	_, err := g.line(append(batchSSH(g), "fetch", "--quiet", "--no-tags", "--prune", "origin",
		"+refs/heads/*:refs/remotes/origin/*")...)
	return err
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
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
func lock(g git) (func(), string, error) {
	gitDir, err := g.line("rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(gitDir, syncLockName)
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, gitDir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) < staleLock {
			return nil, "", fmt.Errorf("%w: %s exists", ErrBusy, path)
		}
		os.Remove(path)
	}
	return nil, "", fmt.Errorf("%w: %s exists", ErrBusy, path)
}

// syncLockName is fleetd's own lock, in the clone's git directory.
const syncLockName = "fleetd-sync.lock"

// clearStaleLocks removes git's lock files in the git directory that are older
// than staleLock, and returns their paths relative to it. git writes a file by
// creating <file>.lock and renaming it into place; a git command killed on a
// timeout leaves the .lock, and git never removes it, so every later command
// that needs the file fails until someone deletes it. The caller holds fleetd's
// sync lock on a clone fleetd owns, so a lock this old was left by a killed
// command, not taken by a running one. The object store is not searched: the gc
// pack runs takes no lock there, and git's automatic maintenance, which would,
// is off.
func clearStaleLocks(gitDir string) []string {
	var cleared []string
	_ = filepath.WalkDir(gitDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(gitDir, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if rel == "objects" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".lock") || rel == syncLockName {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || time.Since(info.ModTime()) < staleLock {
			return nil
		}
		if os.Remove(path) == nil {
			cleared = append(cleared, rel)
		}
		return nil
	})
	return cleared
}

// SameDir reports whether two paths name the same directory, after resolving
// symbolic links; on Windows letter case does not matter.
func SameDir(a, b string) bool {
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
