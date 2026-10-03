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
	// ErrBadFleetFile means FleetFile exists but is not a regular file of a
	// sensible size, cannot be read, is not valid JSON, or holds no salt.
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
	// ErrNoBranch means the branch a person named for the journal is not one
	// the repository has.
	ErrNoBranch = errors.New("gitsync: the repository has no such branch")
	// ErrJournalElsewhere means the branch init would start the journal on holds
	// none, while another of the repository's branches holds one.
	ErrJournalElsewhere = errors.New("gitsync: the journal is on another branch")
	// ErrTwoJournals means two machines made two branches the journal's at once,
	// each with its own salt.
	ErrTwoJournals = errors.New("gitsync: the journal was started on two branches at once")
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
	data, err := readFleetFile(path)
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

// maxFleetFileBytes bounds FleetFile, which holds a few lines.
const maxFleetFileBytes = 64 << 10

// readFleetFile reads FleetFile at path. FleetFile comes from the remote, so any
// machine able to push can make it a link, a directory, a FIFO or a file of any
// size. Anything but a regular file of at most maxFleetFileBytes is
// ErrBadFleetFile: not trusted, so the salt the file last held is used, and a
// sync can still bring in a fixed one. A missing file is os.ErrNotExist.
func readFleetFile(path string) ([]byte, error) {
	data, err := readRegular(path, maxFleetFileBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %v", ErrBadFleetFile, err)
	}
	return data, err
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
	// Branch, when set, is the journal's branch, named by a person where Init
	// cannot tell which it is: after the remote renamed or deleted the branch a
	// clone followed, or with several branches and no default to go by. Init
	// puts the clone there, and refuses a name the repository does not have,
	// unless it has no branch at all: then the journal starts on it.
	Branch string
	// Run runs git; nil means Git.
	Run Runner
}

// InitResult says what Init did.
type InitResult struct {
	// Started is set when the repository was empty and Init made its first commit.
	Started bool `json:"started"`
	// WroteFleetFile is set when Init committed FleetFile to the repository.
	WroteFleetFile bool `json:"wrote_fleet_file"`
	// Cloned is set when Dir was not a clone and now is.
	Cloned bool `json:"cloned"`
	// Branch is the branch the clone follows.
	Branch string `json:"branch"`
	// Head is the remote commit Init found FleetFile in.
	Head string `json:"head"`
	// Reattached is set when Dir was a clone a person had moved off the journal's
	// branch, and Init put it back on it, leaving its files as they were.
	Reattached bool `json:"reattached"`
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
// the next sync. A clone a person moved off the journal's branch is put back on
// it first, moving refs only: on the branch Branch names, when it is set. A clone
// with no commit and no ref of any kind, such as a plain clone of the repository
// made while it was empty, is refused while the repository is still empty, with
// advice that works.
//
// A repository without FleetFile gets it in a commit of its own, pushed at once;
// an empty repository gets it as its first commit. One that already holds
// records needs the salt its machines use, and one with files a journal never
// has is refused, both before anything is pushed; so is a branch holding no
// journal while another branch holds one, where a journal started with a salt
// of its own would split the fleet's. When another machine pushes
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
	// An init killed after its push left the look it made due only in its own
	// clone, which this one replaces: this one makes it, and only then removes
	// that clone.
	lookDue := abandonedLookDue(o.Dir)
	tmp, err := os.MkdirTemp(parent, filepath.Base(o.Dir)+".init-")
	if err != nil {
		return res, err
	}
	// Init's own clone. Once its git directory has moved into Dir it is empty.
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(tmp)
		}
	}()

	g := git{ctx: ctx, dir: parent, run: run}
	clone := append(batchSSH(g), "-c", "init.defaultBranch="+unnamedDefault, "clone", "--quiet", "--no-checkout", "--no-tags", "--", url, tmp)
	if _, err := g.line(clone...); err != nil {
		return res, err
	}
	g.dir = tmp
	if o.Branch != "" {
		if _, err := originHas(g, o.Branch, o.URL); err != nil {
			return res, err
		}
		if _, err := g.line("symbolic-ref", "HEAD", "refs/heads/"+o.Branch); err != nil {
			return res, err
		}
		res.Branch = o.Branch
	} else if res.Branch, err = startBranch(g, o.URL, o.Dir); err != nil {
		return res, err
	}
	tmpGit := filepath.Join(tmp, ".git")
	if err := recordBranch(ctx, tmpGit, res.Branch); err != nil {
		return res, err
	}
	if lookDue {
		if err := os.WriteFile(filepath.Join(tmpGit, lookName), []byte(res.Branch+"\n"), 0o600); err != nil {
			return res, err
		}
	}
	if err := bootstrap(g, tmpGit, o, &res); err != nil {
		// Once its push has made a branch the journal's, or a look found another
		// journal, a look is due, which only the clone notes: kept, as it is, in
		// Dir, it has init run again finish setting it up, and look, and every
		// sync wait till then. A push that failed may have gone through all the
		// same, as when the connection drops after the remote took it: that
		// clone stays beside Dir, as a killed init's does, for the next init to
		// make the look, unless the remote declined the push.
		if _, statErr := os.Lstat(filepath.Join(tmpGit, lookName)); statErr == nil {
			moved := (res.WroteFleetFile || errors.Is(err, ErrTwoJournals)) && os.MkdirAll(o.Dir, 0o755) == nil &&
				RenameRetry(context.WithoutCancel(ctx), tmpGit, filepath.Join(o.Dir, ".git")) == nil
			keep = !moved && !errors.Is(err, ErrRejected)
		}
		return res, err
	}
	// Any look due is made: the clones earlier inits left beside Dir can go.
	removeAbandonedClones(o.Dir)
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
	lockPath := filepath.Join(tmpGit, syncLockName)
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return res, err
	}
	if err := placeFleetFile(g, o.Dir, tmpGit, res.Head); err != nil {
		return res, err
	}
	if err := RenameRetry(ctx, tmpGit, filepath.Join(o.Dir, ".git")); err != nil {
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
	if origin == "" {
		return res, fmt.Errorf("%w: %s has no remote named origin, the one fleetd syncs with; if one of its remotes is "+
			"%s, name it origin (git remote rename), then run fleetd init again", ErrOtherRemote, o.Dir, url)
	}
	if !sameURL(origin, url) {
		return res, fmt.Errorf("%w: %s follows %s, not %s", ErrOtherRemote, o.Dir, origin, url)
	}
	unlock, gitDir, err := lock(g)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := fetchOrigin(g); err != nil {
		return res, err
	}
	if res.Branch, res.Reattached, err = onBranch(g, gitDir, url, o); err != nil {
		return res, err
	}
	// An init stopped after moving its clone's git directory in, and before
	// filling the index, left a clone with no index: every file would look
	// deleted, and untracked, to git. Filling it now finishes that init.
	if _, err := os.Stat(filepath.Join(gitDir, "index")); errors.Is(err, os.ErrNotExist) {
		if _, err := g.line("read-tree", "HEAD"); err != nil {
			return res, err
		}
	}
	// A look that an init in a new directory left due in its clone beside this
	// one, killed after its push or kept there after a push that failed, is
	// made here too, and only then are such clones removed.
	if abandonedLookDue(o.Dir) {
		if err := os.WriteFile(filepath.Join(gitDir, lookName), []byte(res.Branch+"\n"), 0o600); err != nil {
			return res, err
		}
	}
	if err := bootstrap(g, gitDir, o, &res); err != nil {
		return res, err
	}
	removeAbandonedClones(o.Dir)
	if err := adoptFleetFile(g, o.Dir, gitDir, res.Head); err != nil {
		return res, err
	}
	if res.Restored, err = restoreMissing(g); err != nil {
		return res, err
	}
	return res, recordBranch(ctx, gitDir, res.Branch)
}

// onBranch returns the branch of origin's that the clone follows. A clone a person
// moved off the journal's branch is put back on it first: detached, on an orphan
// branch, on a branch that follows nothing, a branch of the clone or another
// remote's, or with no commit yet, as a plain clone made while the repository was
// empty has. Only refs and the index change, never a file, since this machine's
// records are in the work tree and the commands that move a branch would refuse,
// or overwrite them, when the branch's files lack them: restoreMissing then brings
// back the files the work tree lacks, and init's sync the rest. Nothing moves in a
// repository that is not a journal, onto a branch holding no journal while another
// holds one, in the middle of a rebase, merge, cherry-pick, revert or bisect, with
// a commit only HEAD holds, or with the journal's branch checked out in another
// worktree. The journal's branch here starts at origin's
// tip, and moves only forward to it; one with commits of its own stays where it
// is, for the sync to report them. A clone made with --single-branch is set to
// fetch the journal's branch too. HEAD moves last, so that a repair cut short
// leaves a clone off the branch, which init repairs again. A current branch that
// follows one of origin's the clone no longer has is left for a person: the
// remote may have deleted or renamed it, and init would not know where the
// journal went, unless the person names its branch, in o.Branch. The branch the
// clone is put on is recorded as the journal's once nothing stops the move,
// before any ref moves: a clone on a branch following another of origin's than
// the one named, else recorded, is put back on that one too.
func onBranch(g git, gitDir, url string, o InitOptions) (branch string, reattached bool, err error) {
	named := o.Branch
	recorded, err := recordedBranch(gitDir)
	if err != nil {
		return "", false, err
	}
	want := named
	if want == "" {
		want = recorded
	}
	if upstream, err := g.line("rev-parse", "--symbolic-full-name", "@{u}"); err == nil {
		if theirs, ok := strings.CutPrefix(upstream, "refs/remotes/origin/"); ok && (want == "" || theirs == want) {
			if _, err := g.line("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
				return theirs, false, nil
			}
		}
	}
	if err := g.ctx.Err(); err != nil {
		return "", false, err
	}
	head, _ := g.line("symbolic-ref", "--quiet", "HEAD")
	if named != "" {
		// A person said which branch is the journal's: no guess, and no refusal of
		// a branch that follows one the remote no longer has.
		has, err := originHas(g, named, url)
		if err != nil {
			return "", false, err
		}
		branch = named
		if !has {
			// origin has no branch at all, which journalBranch says what to do about.
			if _, err := journalBranch(g, url, named, recorded); err != nil {
				return "", false, err
			}
		}
	} else {
		if head != "" {
			theirs, err := goneUpstream(g, head)
			if err != nil {
				return "", false, err
			}
			// A gone branch the record does not name, as a stray a person switched
			// to, is not the journal's: the record says where the journal is.
			if theirs != "" && (recorded == "" || recorded == theirs) {
				return "", false, goneError(g, head, theirs)
			}
		}
		if branch, err = journalBranch(g, url, "", recorded); err != nil {
			return "", false, err
		}
	}
	if _, err := g.line("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err != nil {
		// A branch made after init's fetch.
		if _, err := g.line(append(batchSSH(g), "fetch", "--quiet", "--no-tags", "origin",
			"+refs/heads/"+branch+":refs/remotes/origin/"+branch)...); err != nil {
			return "", false, err
		}
	}
	tip, err := g.line("rev-parse", "--verify", "refs/remotes/origin/"+branch)
	if err != nil {
		return "", false, err
	}
	top, err := topLevel(g, tip)
	if err != nil {
		return "", false, err
	}
	if err := looksLikeJournal(top); err != nil {
		return "", false, fmt.Errorf("%w (%s): %v", ErrNotJournal, url, err)
	}
	if err := journalElsewhere(g, url, g.dir, branch, top); err != nil {
		return "", false, err
	}
	// A salt given with --salt that differs from the journal's is refused before
	// the clone moves onto it, not after.
	if _, ok := top[FleetFile]; ok && o.Strict {
		data, err := fleetBlob(g, tip, url)
		if err != nil {
			return "", false, err
		}
		fleet, err := parseFleet([]byte(data), url+"'s "+FleetFile)
		if err != nil {
			return "", false, err
		}
		if fleet.Salt != o.Salt {
			return "", false, ErrSaltMismatch
		}
	}
	if err := leftAsItIs(g, gitDir, head, branch); err != nil {
		return "", false, err
	}
	// Recorded before anything moves, so that init run again, with --branch or
	// without, finishes a move cut short or refused part way, and sync till then
	// says to.
	if err := recordBranch(g.ctx, gitDir, branch); err != nil {
		return "", false, err
	}
	ref := "refs/heads/" + branch
	target, old := tip, ""
	if have, err := g.line("rev-parse", "--verify", "--quiet", ref); err == nil {
		old = have
		if _, err := g.line("merge-base", "--is-ancestor", have, tip); err != nil {
			if err := g.ctx.Err(); err != nil {
				return "", false, err
			}
			target = have
		}
	}
	steps := [][]string{{"read-tree", target}}
	if target != old {
		steps = append(steps, []string{"update-ref", ref, target, old})
	}
	// The upstream's branch before its remote: cut short between the two, the
	// branch follows itself, or one of another remote's, which init repairs
	// again, never one of origin's it might take for gone.
	steps = append(steps,
		[]string{"config", "--replace-all", "branch." + branch + ".merge", ref},
		[]string{"config", "--replace-all", "branch." + branch + ".remote", "origin"})
	for _, args := range steps {
		if _, err := g.line(args...); err != nil {
			return "", false, err
		}
	}
	// A clone made with --single-branch fetches its own branch alone: git takes
	// origin's branch for the upstream only once the clone's refspec covers it, so
	// the refspec's branch lines give way to one for every branch of origin's, as
	// a clone has. Its other lines, notes or a branch left out on purpose, stay.
	// The upstream is read as for-each-ref gives it, by the branch's full name,
	// never as <branch>@{upstream}, which git takes for HEAD's for a branch named @.
	upstream := func() (string, error) {
		ups, err := upstreams(g)
		return ups[branch], err
	}
	up, err := upstream()
	if err != nil {
		return "", false, err
	}
	if up != "refs/remotes/origin/"+branch {
		if _, err := g.line("config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*",
			`^\+?refs/heads/`); err != nil {
			return "", false, err
		}
		// A line init does not rewrite, in a file the clone's config includes,
		// for the fetch or for the branch itself, may still decide it; every sync
		// would then say to run init, which would say all was well.
		if up, err = upstream(); err != nil {
			return "", false, err
		}
		if up != "refs/remotes/origin/"+branch {
			return "", false, fmt.Errorf("%w: git takes origin's %s to %q, not to refs/remotes/origin/%s, as a line "+
				"in a file %s's config includes says, for remote.origin.fetch or for the branch; remove it (`git -C \"%s\" "+
				"config --show-origin --list` shows each line and its file), then run fleetd init again, which finishes "+
				"putting the clone back; till then HEAD stays where it was", ErrNoUpstream, branch, up, branch, g.dir, g.dir)
		}
	}
	if _, err := g.line("symbolic-ref", "HEAD", ref); err != nil {
		return "", false, err
	}
	return branch, true, nil
}

// leftAsItIs refuses to move a clone a person is in the middle of something in:
// a rebase, merge, cherry-pick, revert or bisect, a commit only HEAD holds, or the
// journal's branch checked out in another worktree, which would then hold a branch
// that moved under it.
func leftAsItIs(g git, gitDir, head, branch string) error {
	for _, name := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return fmt.Errorf("%w: %s is in the middle of a rebase, merge, cherry-pick, revert or bisect; finish or abort "+
				"it, then run fleetd init again", ErrNoUpstream, g.dir)
		}
	}
	if head == "" {
		if commit, err := g.line("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
			holders, err := g.line("for-each-ref", "--contains", commit, "--format=%(refname)")
			if err != nil {
				return err
			}
			if holders == "" {
				return fmt.Errorf("%w: %s's HEAD is at %s, a commit no branch or tag holds; keep it on a branch, "+
					"`git -C \"%s\" branch fleetd-kept-%s %s`, then run fleetd init again; the branch can go once the "+
					"commit is not wanted", ErrNoUpstream, g.dir, commit, g.dir, commit[:12], commit)
			}
		}
	}
	list, err := g.line("worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, block := range strings.Split(list, "\n\n") {
		var path, checked string
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			} else if b, ok := strings.CutPrefix(line, "branch "); ok {
				checked = b
			}
		}
		if checked == "refs/heads/"+branch && path != "" && !SameDir(path, g.dir) {
			return fmt.Errorf("%w: %s, the journal's branch, is checked out in the worktree at %s; run fleetd init again "+
				"once it is not", ErrNoUpstream, branch, path)
		}
	}
	return nil
}

// journalBranch names the journal's branch for a clone that does not follow it:
// the branch init recorded when it set the clone up, while origin has it, and
// none if origin does not, since only a person knows where the journal went; for
// a clone with no record, a branch this clone has followed, of the same name on
// origin, if origin still has it, since a remote's default can change while its
// machines go on publishing to the branch they follow; else the remote's default,
// as init's own clone would follow; else origin's only branch. With several such
// branches, none of them the default, the journal could be on any, and a person
// says which. A repository with no branch at all has no journal to follow yet: a
// clone with no commit and no ref, such as a plain clone made while the
// repository was empty, holds nothing in its git directory, and without it init
// sets the directory up as a new one, starting the journal; a clone with commits
// of its own keeps them in its git directory, moved aside. named is the branch a
// person named, for that advice, and recorded the one init recorded.
func journalBranch(g git, url, named, recorded string) (string, error) {
	out, err := g.line(append(batchSSH(g), "ls-remote", "--symref", "origin", "HEAD", "refs/heads/*")...)
	if err != nil {
		return "", err
	}
	def := ""
	heads := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if ref, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
			// The remote's HEAD, never a branch the remote keeps as a symbolic ref.
			if target, name, _ := strings.Cut(ref, "\t"); name == "HEAD" {
				def = target
			}
		} else if _, ref, ok := strings.Cut(l, "\t"); ok {
			if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
				heads[name] = true
			}
		}
	}
	if recorded != "" && named == "" {
		if heads[recorded] {
			return recorded, nil
		}
		return "", recordedGone(g, url, recorded)
	}
	ups, err := upstreams(g)
	if err != nil {
		return "", err
	}
	var followed []string
	for name, upstream := range ups {
		if upstream == "refs/remotes/origin/"+name && heads[name] {
			followed = append(followed, name)
		}
	}
	slices.Sort(followed)
	switch {
	case slices.Contains(followed, def):
		return def, nil
	case len(followed) == 1:
		return followed[0], nil
	case len(followed) > 1:
		// The fleet may publish to any of them, the remote's default having moved
		// since, so none is taken for the journal's.
		why, named := ErrNoDefaultBranch, "names no default branch that exists"
		if heads[def] {
			why, named = ErrNoUpstream, "has "+def+" for its default"
		}
		return "", fmt.Errorf("%w: this clone follows %s on %s, which %s, so the journal could be on any of them: "+
			"`fleetd init --dir \"%s\" --branch <branch> <journal URL>` puts the clone on the one it is on",
			why, strings.Join(followed, ", "), url, named, g.dir)
	}
	if heads[def] {
		return def, nil
	}
	var branches []string
	for name := range heads {
		branches = append(branches, name)
	}
	slices.Sort(branches)
	switch {
	case len(branches) == 1:
		return branches[0], nil
	case len(branches) > 1:
		defaultIs := "names no branch"
		if def != "" {
			defaultIs = "is " + def + ", which does not exist"
		}
		return "", fmt.Errorf("%w: %s's default branch %s, but it has %s; make the branch that holds the journal "+
			"its default, or name it, `fleetd init --dir \"%s\" --branch <branch> <journal URL>`", ErrNoDefaultBranch,
			url, defaultIs, strings.Join(branches, ", "), g.dir)
	}
	if err := g.ctx.Err(); err != nil {
		return "", err
	}
	// Nothing of its own: no commit on HEAD, and no ref at all, not a branch, a tag
	// or another remote's branch, holding one.
	_, headErr := g.line("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	refs, err := g.line("for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil {
		return "", err
	}
	again := "run fleetd init again"
	if named != "" {
		again = "run fleetd init again with the same --branch"
	}
	if refs == "" && headErr != nil {
		return "", fmt.Errorf("%w: %s has no commit, as a clone made while the journal repository was empty has none; "+
			"delete its .git directory, then %s, which sets it up and keeps every other file there",
			ErrNoUpstream, g.dir, again)
	}
	return "", fmt.Errorf("%w: %s has no branch, and %s has commits of its own; move its .git directory, and every "+
		"file there but journal files and %s, out of it, then %s, which starts the journal there",
		ErrNoUpstream, url, g.dir, FleetFile, again)
}

// upstreams maps each branch here to the full name of the branch it follows, or
// to "" for one that follows none.
func upstreams(g git) (map[string]string, error) {
	out, err := g.line("for-each-ref", "--format=%(refname) %(upstream)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		ref, upstream, _ := strings.Cut(line, " ")
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			m[name] = upstream
		}
	}
	return m, nil
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
	want, err := fleetBlob(g, tip, "the journal")
	if err != nil {
		return err
	}
	w, err := parseFleet([]byte(want), FleetFile)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FleetFile)
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		return fmt.Errorf("%w: %s is a directory; remove it, then run fleetd init again", ErrBadFleetFile, path)
	}
	// One that is not a regular file to trust, a link say, has no salt to note,
	// and is replaced below.
	if have, err := readFleetFile(path); err == nil {
		if f, perr := parseFleet(have, path); perr == nil && f.Salt != w.Salt {
			if err := NotePastSalt(gitDir, f.Salt); err != nil {
				return err
			}
		}
	}
	// "<mode> blob <id>\t<path>" from the tree, "<mode> <id> 0\t<path>" from the index.
	entry, err := g.line("ls-tree", tip, "--", FleetFile)
	if err != nil {
		return err
	}
	mode, rest, _ := strings.Cut(entry, " ")
	id, _, _ := strings.Cut(strings.TrimPrefix(rest, "blob "), "\t")
	// git undoes its own conversions when it hashes the file: CRLF line endings,
	// and whatever a .gitattributes asks for, such as ident or another encoding.
	// The file is the journal's only if fleetd also reads the journal's salt from
	// it as it stands; a conversion that hides it is undone by writing git's copy.
	same := false
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		got, err := g.line("hash-object", "--", FleetFile)
		if err != nil {
			return err
		}
		f, ok, rerr := ReadFleet(dir)
		same = got == id && rerr == nil && ok && f.Salt == w.Salt
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
	// --replace drops what the index holds under a FleetFile directory a sync
	// brought in, which the person has removed from the work tree.
	_, err = g.line("update-index", "--add", "--replace", "--cacheinfo", mode+","+id+","+FleetFile)
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

// abandonedLookDue reports whether a clone an init left beside dir, as one
// killed after its push does, notes a look as due.
func abandonedLookDue(dir string) bool {
	due, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".init-*", ".git", lookName))
	return len(due) > 0
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
func startBranch(g git, url, dir string) (string, error) {
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
			"its default, or name it, `fleetd init --dir \"%s\" --branch <branch> <journal URL>`", ErrNoDefaultBranch,
			url, named, strings.Join(others, ", "), dir)
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

// originHas checks the branch a person named for the journal: a branch name,
// and one origin has, else an error naming the branches origin does have. With
// origin holding no branch at all, it reports false and no error.
func originHas(g git, name, url string) (bool, error) {
	if _, err := g.line("check-ref-format", "--branch", name); err != nil {
		if err := g.ctx.Err(); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%w: %q is not a branch name", ErrNoBranch, name)
	}
	if _, err := g.line("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+name); err == nil {
		return true, nil
	}
	out, err := g.line("for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/")
	if err != nil {
		return false, err
	}
	var branches []string
	for _, b := range strings.Split(out, "\n") {
		if b != "" && b != "HEAD" {
			branches = append(branches, b)
		}
	}
	if len(branches) == 0 {
		return false, nil
	}
	return false, fmt.Errorf("%w: %s has no branch %s; it has %s", ErrNoBranch, url, name, strings.Join(branches, ", "))
}

// recordBranch records branch as the journal's in the clone whose git directory
// is gitDir, by renaming in a file written beside the record, so that no reader
// sees half of one.
func recordBranch(ctx context.Context, gitDir, branch string) error {
	f, err := os.CreateTemp(gitDir, "."+branchName+".*")
	if err != nil {
		return err
	}
	// The reader strips one byte order mark, as an editor may add; a name that
	// starts with one keeps it.
	if strings.HasPrefix(branch, "\ufeff") {
		branch = "\ufeff" + branch
	}
	_, err = f.WriteString(branch + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = RenameRetry(ctx, f.Name(), filepath.Join(gitDir, branchName))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// recordedGone says what to do about the journal's recorded branch, gone from
// origin, in a clone that is not on a branch following it.
func recordedGone(g git, url, recorded string) error {
	back, err := pushBack(g, recorded)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is the journal's branch, the one init set this clone up on, and %s no longer has it, as "+
		"when the remote deleted or renamed it. If it was renamed, or deleted on purpose, `fleetd init --dir \"%s\" "+
		"--branch <branch> <journal URL>` puts the clone on <branch>, the one the journal is on now; if it was deleted "+
		"by mistake, push it back from the machine that synced last, as fleetd sync there says; %s; then run fleetd "+
		"init here again", ErrNoUpstream, recorded, url, g.dir, back)
}

// journalElsewhere refuses to start the journal on branch, whose top level is
// top, when branch holds none, neither FleetFile nor a host journal file, while
// another of origin's branches holds one: the fleet publishes there, and a
// journal started here, with a salt of its own, would split it. dir is the
// journal directory, for the advice.
func journalElsewhere(g git, url, dir, branch string, top map[string]string) error {
	if holdsJournal(top) {
		return nil
	}
	holding, err := journalsBut(g, branch)
	if err != nil || len(holding) == 0 {
		return err
	}
	on := holding[0]
	if len(holding) > 1 {
		on = "each of " + strings.Join(holding, ", ")
	}
	return fmt.Errorf("%w: %s holds no journal on %s, but holds one on %s, where the fleet publishes; a journal started "+
		"on %s would split it. `fleetd init --dir \"%s\" --branch <branch> <journal URL>` follows the one on <branch>",
		ErrJournalElsewhere, url, branch, on, branch, dir)
}

// journalsBut names the branches of origin's, as this clone last fetched them,
// that hold a journal, but for branch.
func journalsBut(g git, branch string) ([]string, error) {
	out, err := g.line("for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/")
	if err != nil {
		return nil, err
	}
	var holding []string
	for _, name := range strings.Split(out, "\n") {
		if name == "" || name == "HEAD" || name == branch {
			continue
		}
		top, err := topLevel(g, "refs/remotes/origin/"+name)
		if err != nil {
			return nil, err
		}
		if holdsJournal(top) {
			holding = append(holding, name)
		}
	}
	return holding, nil
}

// holdsJournal reports whether a commit's top level holds a journal: FleetFile,
// or a host journal file, as a journal of fleetd v0.1.0's holds without one.
func holdsJournal(top map[string]string) bool {
	for name, kind := range top {
		if kind == "blob" && (name == FleetFile || hostFile.MatchString(name)) {
			return true
		}
	}
	return false
}

// bootstrap makes sure the remote branch has FleetFile, and sets res.Head and
// res.Salt from it. It moves refs only, never a work tree. A push that makes a
// branch holding no journal the journal's, or starts one, makes a look at
// origin's other branches due, noted in gitDir, the clone's git directory: made
// on the branches the fetch after the push brings, and by every init until one
// finds no other journal, it is made even when that fetch fails, or an init
// stops before it.
func bootstrap(g git, gitDir string, o InitOptions, res *InitResult) error {
	ssh := batchSSH(g)
	for attempt := 1; ; attempt++ {
		tip, _ := g.line("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+res.Branch)
		if tip == "" && attempt > 1 {
			// Another machine's push beat this one's, so the branch was there; gone
			// since, it was deleted or renamed, and starting it again from nothing
			// would split the journal.
			if err := g.ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s no longer has %s, the branch init was setting the journal up on, as when it was "+
				"deleted or renamed meanwhile; nothing was pushed, and fleetd init run again looks at it afresh",
				ErrNoUpstream, o.URL, res.Branch)
		}
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
				data, err := fleetBlob(g, tip, o.URL)
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
				if _, err := os.Lstat(filepath.Join(gitDir, lookName)); err == nil {
					return look(g, gitDir, res.Branch)
				}
				return nil
			}
		}
		if attempt > MaxAttempts {
			return fmt.Errorf("gitsync: %s still has no %s after %d attempts", o.URL, FleetFile, MaxAttempts)
		}
		if tip != "" {
			if err := journalElsewhere(g, o.URL, o.Dir, res.Branch, top); err != nil {
				return err
			}
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
		// Starting the journal, the branch has no top level, and holds none.
		if !holdsJournal(top) {
			if err := os.WriteFile(filepath.Join(gitDir, lookName), []byte(res.Branch+"\n"), 0o600); err != nil {
				return err
			}
		}
		// Onto the tip looked at, or, starting the journal, onto no branch at all:
		// a branch renamed or deleted meanwhile refuses it, as a race lost.
		out, err := g.line(append(ssh, "push", "--porcelain", "--no-verify", "--force-with-lease=refs/heads/"+res.Branch+":"+tip,
			"origin", commit+":refs/heads/"+res.Branch)...)
		switch {
		case err == nil:
			res.WroteFleetFile, res.Started = true, tip == ""
		case refused(out):
			return fmt.Errorf("%w (it may protect %s from direct pushes; fleetd needs to push to it): %w", ErrRejected, res.Branch, err)
		case !lostRace(out):
			return err
		}
		// Either way the remote has a tip to look at again: this machine's
		// commit, or the one that beat it.
		if err := fetchOrigin(g); err != nil {
			if _, statErr := os.Lstat(filepath.Join(gitDir, lookName)); statErr == nil && res.WroteFleetFile {
				return couldNotLook(res.Branch, err)
			}
			return err
		}
	}
}

// lookName is the file, in a clone's git directory, that notes a look at
// origin's other branches is due: a push of this clone's may have made branch,
// which held no journal, the journal's. It is never committed.
const lookName = "fleetd-look"

// look fails when another of origin's branches than branch holds a journal
// too, as this clone last fetched them, once a push of this clone's has made
// branch the journal's, starting it, or on a branch that held none: two machines
// did so at once, on two branches, each with its own salt, which the look before
// the push cannot rule out. It notes the look done only when it finds no other
// journal, so that until then every init looks.
func look(g git, gitDir, branch string) error {
	holding, err := journalsBut(g, branch)
	if err != nil {
		return err
	}
	if len(holding) > 0 {
		// A copy of this journal, a branch made from it since, shares a commit
		// holding FleetFile with it; a journal another machine started at the
		// same time shares at most one from before either push, holding none.
		var others []string
		for _, h := range holding {
			base, _ := g.line("merge-base", "refs/remotes/origin/"+branch, "refs/remotes/origin/"+h)
			if base != "" {
				if entry, _ := g.line("ls-tree", base, "--", FleetFile); entry != "" {
					continue
				}
			}
			others = append(others, h)
		}
		if err := g.ctx.Err(); err != nil {
			return err
		}
		if len(others) > 0 {
			return twoBranches(branch, others)
		}
	}
	return os.RemoveAll(filepath.Join(gitDir, lookName))
}

func couldNotLook(branch string, err error) error {
	return fmt.Errorf("gitsync: %s is the journal's branch now, but init could not then look whether another "+
		"machine made another branch the journal's at the same time (%w); fleetd init run again looks again",
		branch, err)
}

// twoBranches says what a person does about a journal started on branch and on
// others at once.
func twoBranches(branch string, others []string) error {
	return fmt.Errorf("%w, %s and %s. Keep the repository's "+
		"default branch: on any machine whose journal follows the other one, delete the journal's .git directory (its "+
		"records stay; left in place, its syncs stop once that branch is gone, until `fleetd init --dir \"<its "+
		"journal directory>\" --branch <branch> <journal URL>` names the one kept); then delete the other branch, "+
		"and run fleetd init again on those machines and on this one", ErrTwoJournals, branch, strings.Join(others, ", "))
}

// placeFleetFile writes the journal's FleetFile into dir, so that a record
// appended from then on uses the journal's salt. dir may hold one already, from
// an interrupted init or written by hand: the journal's replaces it, and a salt
// it held that the journal does not use is noted in gitDir, so that the records
// this machine wrote under it are filed under the journal's later.
func placeFleetFile(g git, dir, gitDir, tip string) error {
	want, err := fleetBlob(g, tip, "the journal")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FleetFile)
	// One that is not a file fleetd reads, too large say, never gave a record its
	// salt: it has no salt to note, and is replaced below.
	have, err := readFleetFile(path)
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
	case !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrBadFleetFile):
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
	data, err := readRegular(path, maxSaltsBytes)
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
	data, _ := readRegular(path, maxSaltsBytes)
	return parseSalts(data)
}

// maxSaltsBytes bounds a salts file, which holds a line per salt.
const maxSaltsBytes = 1 << 20

// fleetBlob returns commit's FleetFile, refusing one too large before reading any
// of it, so that init accepts no fleetd.json that every other command would
// refuse. Checked out with CRLF line endings, as Git for Windows checks text out
// by default, a file can be twice its size in git, so the limit is half of what
// ReadFleet reads. where names the repository in the error.
func fleetBlob(g git, commit, where string) (string, error) {
	size, err := g.line("cat-file", "-s", commit+":"+FleetFile)
	if err != nil {
		return "", err
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return "", fmt.Errorf("gitsync: git gave %q as the size of %s's %s", size, where, FleetFile)
	}
	if n > maxFleetFileBytes/2 {
		return "", fmt.Errorf("%w: %s's %s is larger than %d bytes", ErrBadFleetFile, where, FleetFile, maxFleetFileBytes/2)
	}
	return g.raw(nil, "cat-file", "blob", commit+":"+FleetFile)
}

// readRegular reads the file at path if it is a regular file of at most limit
// bytes, so that one planted there, a large sparse file say, cannot exhaust
// memory.
func readRegular(path string, limit int64) ([]byte, error) {
	f, err := journal.OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("refusing to read %s: it is larger than %d bytes", path, limit)
	}
	return data, nil
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
