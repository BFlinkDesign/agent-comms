//go:build !windows

package journal

import (
	"fmt"
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

// syncDir flushes the directory entry for a newly created journal file. POSIX
// requires this separately from fsyncing the file: the file's own sync persists
// its contents and inode, but not the name that points at them.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	return d.Close()
}

// OpenRegular opens path, a file beside the journal that another account able to
// write there could have replaced, refusing anything but a regular file. A
// symbolic link fails the open (O_NOFOLLOW). A FIFO or a device would make the
// open wait for a reader or a writer that may never come; O_NONBLOCK makes it
// return at once, and the file is then refused. On a regular file O_NONBLOCK has
// no effect.
func OpenRegular(path string, flag int, perm os.FileMode) (*os.File, error) {
	if err := refuseIrregular(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	return checkRegular(f)
}

// refuseIrregular refuses a path that holds anything but a regular file, before
// OpenRegular opens it; a path that holds nothing yet is fine.
func refuseIrregular(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return nil
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("refusing to open the symbolic link %s", path)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("refusing to open %s: it is not a regular file", path)
	}
	return nil
}

// checkRegular returns f if it is a regular file, and closes it otherwise: the
// path may have been replaced between refuseIrregular's look and the open.
func checkRegular(f *os.File) (*os.File, error) {
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("refusing to open %s: it is not a regular file", f.Name())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
