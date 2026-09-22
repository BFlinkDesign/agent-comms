// Package gitsync publishes one machine's journal file through git and brings in
// every other machine's.
//
// The journal directory is a clone of a repository that holds one file per host.
// Sync stages only this host's file, commits it, rebases onto the remote and
// pushes. Because no two hosts ever write the same file, the rebase has nothing to
// merge and cannot conflict; the one way it can is two machines deriving the same
// host id, which is reported as that rather than resolved by guesswork.
//
// Every git command runs under the caller's context, so a hung network or a
// credential prompt cannot hold the process open: prompts are disabled outright.
package gitsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// MaxAttempts bounds how many times a push rejected by a concurrent push from
// another machine is retried.
const MaxAttempts = 3

var (
	// ErrNotClone means the journal directory is not inside a git work tree.
	ErrNotClone = errors.New("gitsync: journal directory is not a git clone")
	// ErrNoUpstream means the current branch tracks no remote branch.
	ErrNoUpstream = errors.New("gitsync: current branch has no upstream")
	// ErrSameFile means the rebase conflicted on this host's own file, which only
	// happens when another machine writes a file with the same host id.
	ErrSameFile = errors.New("gitsync: another machine wrote this host's journal file")
)

// Runner runs git with args in dir and returns its standard output.
type Runner func(ctx context.Context, dir string, args ...string) (string, error)

// Git runs the git binary on PATH. Terminal prompts are disabled so a missing
// credential fails the command instead of waiting forever for input.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

// Options says what to publish.
type Options struct {
	// Dir is the journal directory, inside a clone of the journal repository.
	Dir string
	// File is this host's journal file, inside Dir.
	File string
	// Message is the commit message for this host's new records.
	Message string
	// Run runs git; nil means Git.
	Run Runner
}

// Result says what a sync did.
type Result struct {
	// Published is the number of this host's records committed and pushed.
	Published int `json:"published"`
	// Received is the number of commits from other machines brought in.
	Received int `json:"received"`
	// Head is the commit the local clone is at afterwards.
	Head string `json:"head"`
	// Attempts is how many push attempts it took.
	Attempts int `json:"attempts"`
}

// Sync commits this host's journal file and exchanges commits with the remote.
func Sync(ctx context.Context, o Options) (Result, error) {
	run := o.Run
	if run == nil {
		run = Git
	}
	var res Result

	top, err := run(ctx, o.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return res, fmt.Errorf("%w: %s (%v)", ErrNotClone, o.Dir, err)
	}
	top = strings.TrimSpace(top)
	if _, err := run(ctx, top, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err != nil {
		return res, fmt.Errorf("%w: set one with `git push -u origin <branch>` in %s", ErrNoUpstream, top)
	}

	rel, err := relative(top, o.File)
	if err != nil {
		return res, err
	}
	if _, err := os.Stat(o.File); err == nil {
		// Only this host's file is ever staged or committed. Anything else in the
		// work tree is left exactly as it is.
		if _, err := run(ctx, top, "add", "--", rel); err != nil {
			return res, err
		}
		stat, err := run(ctx, top, "diff", "--cached", "--numstat", "--", rel)
		if err != nil {
			return res, err
		}
		if added := addedLines(stat); added > 0 {
			if _, err := run(ctx, top, "commit", "--quiet", "--no-verify", "-m", o.Message, "--", rel); err != nil {
				return res, err
			}
			res.Published = added
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, err
	}

	for res.Attempts = 1; ; res.Attempts++ {
		if _, err := run(ctx, top, "fetch", "--quiet"); err != nil {
			return res, err
		}
		incoming, err := run(ctx, top, "rev-list", "--count", "HEAD..@{u}")
		if err != nil {
			return res, err
		}
		n, _ := strconv.Atoi(strings.TrimSpace(incoming))
		res.Received += n
		if _, err := run(ctx, top, "rebase", "--quiet", "--autostash", "@{u}"); err != nil {
			// Leave the clone as it was found rather than mid-rebase.
			_, _ = run(ctx, top, "rebase", "--abort")
			if strings.Contains(err.Error(), rel) || strings.Contains(strings.ToLower(err.Error()), "conflict") {
				return res, fmt.Errorf("%w: %s; give each machine a distinct identity (see `fleetd host`)", ErrSameFile, rel)
			}
			return res, err
		}
		ahead, err := run(ctx, top, "rev-list", "--count", "@{u}..HEAD")
		if err != nil {
			return res, err
		}
		if strings.TrimSpace(ahead) == "0" {
			break // nothing of ours to send
		}
		_, err = run(ctx, top, "push", "--quiet")
		if err == nil {
			break
		}
		// Another machine pushed between this fetch and this push. Its commit
		// touches only its own file, so fetching and rebasing again resolves it.
		if res.Attempts >= MaxAttempts {
			return res, fmt.Errorf("gitsync: push still rejected after %d attempts: %w", res.Attempts, err)
		}
	}

	head, err := run(ctx, top, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	res.Head = strings.TrimSpace(head)
	return res, nil
}

// relative returns file relative to top with forward slashes, as git pathspecs
// expect, and refuses a file outside the clone.
func relative(top, file string) (string, error) {
	absTop, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(file))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absTop, filepath.Join(dir, filepath.Base(file)))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gitsync: %s is outside the clone at %s", file, top)
	}
	return filepath.ToSlash(rel), nil
}

// addedLines sums the added-lines column of `git diff --numstat`.
func addedLines(numstat string) int {
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.Atoi(fields[0]); err == nil {
			total += n
		}
	}
	return total
}
