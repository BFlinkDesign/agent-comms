//go:build !windows

package gitsync

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// processGone reports whether a process has ended. A killed process nobody has
// reaped yet is a zombie, which still answers signal 0; on Linux its state says
// it has ended.
func processGone(t *testing.T, pid int) bool {
	t.Helper()
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err == nil {
			// "pid (comm) S ...": the state follows the last ')'.
			rest := string(stat[strings.LastIndexByte(string(stat), ')')+1:])
			if f := strings.Fields(rest); len(f) > 0 && (f[0] == "Z" || f[0] == "X") {
				return true
			}
		}
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
