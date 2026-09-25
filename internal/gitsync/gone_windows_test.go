//go:build windows

package gitsync

import (
	"syscall"
	"testing"
)

const (
	processQueryLimitedInformation = 0x1000            // PROCESS_QUERY_LIMITED_INFORMATION
	errorInvalidParameter          = syscall.Errno(87) // ERROR_INVALID_PARAMETER
)

// processGone reports whether a process has ended: no process has its id any
// more, or it has one and it has exited.
func processGone(t *testing.T, pid int) bool {
	t.Helper()
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|processQueryLimitedInformation, false, uint32(pid))
	if err == errorInvalidParameter {
		return true
	}
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer syscall.CloseHandle(h)
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		t.Fatalf("WaitForSingleObject: %v", err)
	}
	return event == syscall.WAIT_OBJECT_0
}
