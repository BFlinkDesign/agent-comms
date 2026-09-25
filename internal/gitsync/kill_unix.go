//go:build !windows

package gitsync

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// runTree runs the git command newCmd makes in a process group of its own and,
// when the context ends, kills the whole group, so a child such as ssh cannot
// keep the sync waiting.
func runTree(newCmd func() *exec.Cmd) error {
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The whole group has ended already. Say so as exec's own Cancel
			// does, so that exec reports git's exit status, not a failed kill.
			return os.ErrProcessDone
		}
		return err
	}
	return cmd.Run()
}
