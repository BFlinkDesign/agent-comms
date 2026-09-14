//go:build windows

package journal

import "os"

// openAppend opens the journal for appending.
//
// Windows has no O_NOFOLLOW, so the caller's Lstat check is the only symlink
// guard available and a narrow race remains between the check and this open. That
// is stated rather than papered over. In practice creating a symlink on Windows
// requires either administrator rights or Developer Mode, so an unprivileged
// attacker cannot set the trap in the first place; a privileged one has better
// options than racing a journal file.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
