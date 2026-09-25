package gitsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FleetFile is the journal repository's own settings file. It holds the fleet's
// salt, so every machine that clones the journal derives host ids the same way
// without each one having to be given the salt: a hook started without
// FLEET_SALT in its environment would otherwise file its records under a second
// id for the same machine, which nothing ever publishes.
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
	// ErrBadFleetFile means FleetFile exists but holds no salt.
	ErrBadFleetFile = errors.New("gitsync: fleetd.json holds no salt")
)

// ReadFleet reads FleetFile from a journal directory; ok is false when it has none.
func ReadFleet(dir string) (f Fleet, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, FleetFile))
	if errors.Is(err, os.ErrNotExist) {
		return Fleet{}, false, nil
	}
	if err != nil {
		return Fleet{}, false, err
	}
	if err := json.Unmarshal(data, &f); err != nil || strings.TrimSpace(f.Salt) == "" {
		return Fleet{}, false, fmt.Errorf("%w: %s", ErrBadFleetFile, filepath.Join(dir, FleetFile))
	}
	return f, true, nil
}

// InitOptions says which journal repository to clone, and where to.
type InitOptions struct {
	// URL is the journal repository.
	URL string
	// Dir is where the clone goes. It must not exist, or be an empty directory.
	Dir string
	// Salt is a salt the caller was given. A journal without FleetFile gets it,
	// or a random one when it is empty; a journal with FleetFile must match it.
	Salt string
	// Run runs git; nil means Git.
	Run Runner
}

// InitResult says what Init did.
type InitResult struct {
	// Started is set when the repository was empty and Init made its first commit.
	Started bool `json:"started"`
	// WroteFleetFile is set when Init committed fleetd.json.
	WroteFleetFile bool `json:"wrote_fleet_file"`
	// Branch is the branch the clone follows.
	Branch string `json:"branch"`
	// Head is the commit the clone is at, which is the remote's.
	Head string `json:"head"`
	// Salt is the journal's salt. It is not a credential, but it is nobody's
	// business either, so it is not printed.
	Salt string `json:"-"`
}

// Init clones a journal repository into o.Dir and makes sure it has FleetFile.
// A repository without one gets it in a commit of its own, pushed at once; an
// empty repository gets it as its first commit. When another machine pushes
// first, Init takes what that machine pushed and looks again, at most
// MaxAttempts times. A repository with files a journal never has is refused
// before anything is pushed to it.
func Init(ctx context.Context, o InitOptions) (InitResult, error) {
	var res InitResult
	g := git{ctx: ctx, dir: filepath.Dir(o.Dir), run: o.Run}
	if g.run == nil {
		g.run = Git
	}
	if _, err := g.line(append(batchSSH(g), "clone", "--quiet", "--no-tags", "--", o.URL, o.Dir)...); err != nil {
		return res, err
	}
	g.dir = o.Dir
	ssh := batchSSH(g)
	branch, err := g.line("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return res, fmt.Errorf("gitsync: the clone of %s has no branch checked out: %w", o.URL, err)
	}
	res.Branch = branch

	for attempt := 1; ; attempt++ {
		head, _ := g.line("rev-parse", "--verify", "--quiet", "HEAD")
		if head != "" {
			if err := looksLikeJournal(g, head); err != nil {
				return res, fmt.Errorf("%w (%s): %v", ErrNotJournal, o.URL, err)
			}
			fleet, ok, err := ReadFleet(o.Dir)
			if err != nil {
				return res, err
			}
			if ok {
				if o.Salt != "" && o.Salt != fleet.Salt {
					return res, ErrSaltMismatch
				}
				if _, err := g.line("rev-parse", "--verify", "--quiet", "@{upstream}"); err != nil {
					if _, err := g.line("branch", "--quiet", "--set-upstream-to=origin/"+branch); err != nil {
						return res, err
					}
				}
				res.Head, res.Salt = head, fleet.Salt
				return res, nil
			}
		}
		if attempt > MaxAttempts {
			return res, fmt.Errorf("gitsync: %s still has no %s after %d attempts", o.URL, FleetFile, MaxAttempts)
		}
		salt := o.Salt
		if salt == "" {
			if salt, err = randomSalt(); err != nil {
				return res, err
			}
		}
		commit, err := fleetCommit(g, head, salt)
		if err != nil {
			return res, err
		}
		out, err := g.line(append(ssh, "push", "--porcelain", "--no-verify", "origin", commit+":refs/heads/"+branch)...)
		switch {
		case err == nil:
			res.WroteFleetFile, res.Started = true, head == ""
		case !lostRace(out):
			return res, err
		}
		// Either way the remote now has a tip this clone must take: this machine's
		// commit, or the one that beat it. The clone is Init's own and holds
		// nothing else yet, so a hard reset loses nothing.
		if _, err := g.line(append(ssh, "fetch", "--quiet", "--no-tags", "origin")...); err != nil {
			return res, err
		}
		if _, err := g.line("reset", "--quiet", "--hard", "refs/remotes/origin/"+branch); err != nil {
			return res, err
		}
	}
}

// fleetCommit builds a commit, without touching the working tree or the index,
// that adds FleetFile to parent, or that holds only FleetFile when parent is
// empty, meaning the repository has no commits yet.
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

// looksLikeJournal refuses a commit whose top level holds anything but journal
// files, FleetFile, and what GitHub offers to create with a new repository.
func looksLikeJournal(g git, commit string) error {
	listing, err := g.raw(nil, "ls-tree", "-z", "--name-only", commit)
	if err != nil {
		return err
	}
	var foreign []string
	for _, name := range strings.Split(listing, "\x00") {
		lower := strings.ToLower(name)
		switch {
		case name == "", name == FleetFile, strings.HasSuffix(lower, ".jsonl"),
			strings.HasPrefix(lower, "readme"), strings.HasPrefix(lower, "license"),
			lower == ".gitignore", lower == ".gitattributes":
		default:
			foreign = append(foreign, name)
		}
	}
	if len(foreign) > 0 {
		if len(foreign) > 5 {
			foreign = append(foreign[:5], "…")
		}
		return fmt.Errorf("it holds %s", strings.Join(foreign, ", "))
	}
	return nil
}

func randomSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
