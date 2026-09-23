//go:build windows

package gitsync

import "os/exec"

// killTree relies on the default cancellation, which kills git itself. A child it
// started can outlive it; cmd.WaitDelay still returns control to the caller.
func killTree(cmd *exec.Cmd) {}
