//go:build !windows

package journal

import (
	"os"
	"syscall"
)

// openAppend opens the journal for appending, refusing to follow a symlink.
//
// O_NOFOLLOW closes the window that the caller's Lstat check alone leaves open:
// between the check and the open, the path could be replaced with a link pointing
// somewhere else. With the flag set the open itself fails instead, so there is no
// window to exploit. This mirrors the guard hive/shell_write.py already applies to
// channel files.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW,
		0o600)
}
