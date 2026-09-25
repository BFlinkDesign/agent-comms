//go:build !windows

package gitsync

import (
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
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd.Run()
}
