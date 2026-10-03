//go:build windows

package journal

import (
	"fmt"
	"os"
)

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
// write there could have replaced, refusing a symbolic link or a directory.
// Windows keeps its named pipes outside the file system, so no open of a path
// there can wait on one. Other reparse points, such as OneDrive's placeholders,
// which Go reports as irregular, are files to open; the Lstat check is again the
// only symlink guard.
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

// refuseIrregular refuses a path that holds a symbolic link or a directory,
// before OpenRegular opens it; a path that holds nothing yet is fine.
func refuseIrregular(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return nil
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("refusing to open the symbolic link %s", path)
	case fi.IsDir():
		return fmt.Errorf("refusing to open %s: it is a directory", path)
	}
	return nil
}

// checkRegular returns f unless it is a directory, which it closes.
func checkRegular(f *os.File) (*os.File, error) {
	fi, err := f.Stat()
	if err == nil && fi.IsDir() {
		err = fmt.Errorf("refusing to open %s: it is a directory", f.Name())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
