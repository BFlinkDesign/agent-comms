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

// syncDir is a no-op on Windows. Opening a directory as a file and flushing it
// is a POSIX construct; NTFS commits the directory entry as part of the metadata
// journal, so there is no equivalent call to make here.
func syncDir(string) error { return nil }

// OpenRegular opens path, a file beside the journal that another account able to
// write there could have replaced, refusing anything but a regular file. Windows
// keeps its named pipes outside the file system, so no open of a path there can
// wait on one; the caller's Lstat check is again the only symlink guard.
func OpenRegular(path string, flag int, perm os.FileMode) (*os.File, error) {
	if err := refuseIrregular(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return checkRegular(f)
}
