//go:build !windows

package gitsync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// The Cancel runTree gives git reports a process group that has already ended
// as os.ErrProcessDone, as exec's own Cancel does for a finished process. exec
// then reports a git that finished just as its context ended with git's own
// exit status, rather than as a kill that failed.
func TestCancellingAGitThatHasFinishedReportsItDone(t *testing.T) {
	requireGit(t)
	var cmd *exec.Cmd
	newCmd := func() *exec.Cmd {
		cmd = exec.CommandContext(context.Background(), "git", "version")
		return cmd
	}
	if err := runTree(newCmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("cancelling a git that has finished returned %v, want os.ErrProcessDone", err)
	}
}
