//go:build windows

package gitsync

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Windows will not delete a directory that a live process has as its working
// directory, and git, and everything it starts, runs in the clone. The tests
// that keep git out of fleetd's job on purpose would otherwise leave processes
// in their TempDirs. These helpers end every such process, and wait until
// Windows lets the TempDirs go, before the testing package removes them.

var (
	procQueryInformationJobObject  = kernel32.NewProc("QueryInformationJobObject")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
)

const jobObjectBasicAccountingInformation = 1 // JOBOBJECTINFOCLASS

// basicAccountingInformation mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION,
// which has the same layout on every architecture.
type basicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// releaseTempDirs makes the test, as its last cleanup before the testing
// package's own, wait until its TempDirs can be removed, and say which
// processes are still running if they cannot. Call it before anything else
// that registers a cleanup, so every other cleanup, such as ending a job or a
// sleeper, runs first.
func releaseTempDirs(t *testing.T) {
	t.Helper()
	// The first TempDir registers the testing package's removal of their
	// common parent; this cleanup is registered after it, so it runs before it.
	parent := filepath.Dir(t.TempDir())
	t.Cleanup(func() {
		deadline := time.Now().Add(60 * time.Second)
		for {
			err := os.RemoveAll(parent)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("the TempDirs are still in use 60s after the test ended: %v\n%s", err, runningProcesses())
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

// testJob returns a function that puts a process in a job object this test
// owns. When the test ends, the job is terminated and the test waits until no
// process is left in it. Call it after releaseTempDirs.
func testJob(t *testing.T) func(pid int) error {
	t.Helper()
	job, err := newKillOnCloseJob()
	if err != nil {
		t.Fatalf("creating the test's job: %v", err)
	}
	t.Cleanup(func() {
		defer syscall.CloseHandle(job)
		if r, _, e := procTerminateJobObject.Call(uintptr(job), killExitCode); r == 0 {
			t.Errorf("TerminateJobObject: %v", e)
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			var info basicAccountingInformation
			r, _, e := procQueryInformationJobObject.Call(uintptr(job), jobObjectBasicAccountingInformation,
				uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
			if r == 0 {
				t.Errorf("QueryInformationJobObject: %v", e)
				return
			}
			if info.ActiveProcesses == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d processes in the test's job are still running 30s after it was terminated", info.ActiveProcesses)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return func(pid int) error { return assignToJob(job, pid) }
}

// outsideFleetdsJob makes fleetd fail to put git in its job, as an outer job
// that forbids nesting would, and puts git in the test's job instead, so that
// what git starts can still be ended when the test does. git is still suspended
// when this runs, so nothing it starts escapes.
func outsideFleetdsJob(t *testing.T) {
	t.Helper()
	adopt := testJob(t)
	saved := assignJob
	t.Cleanup(func() { assignJob = saved })
	assignJob = func(_ syscall.Handle, pid int) error {
		if err := adopt(pid); err != nil {
			return err
		}
		return errors.New("cannot join for this test")
	}
}

// killSleeper ends the sleeper with this process id and waits until it has
// ended. A process that is not this test binary is left alone: if the sleeper
// has already ended, another process may have been given its id.
func killSleeper(t *testing.T, pid int) {
	t.Helper()
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE|syscall.SYNCHRONIZE|processQueryLimitedInformation, false, uint32(pid))
	if err == errorInvalidParameter {
		return // no process has this id any more
	}
	if err != nil {
		t.Errorf("OpenProcess(%d): %v", pid, err)
		return
	}
	defer syscall.CloseHandle(h)
	self, err := os.Executable()
	if err != nil {
		t.Errorf("os.Executable: %v", err)
		return
	}
	if image, err := processImage(h); err != nil || !strings.EqualFold(image, self) {
		return
	}
	if err := syscall.TerminateProcess(h, killExitCode); err != nil {
		t.Errorf("TerminateProcess(%d): %v", pid, err)
		return
	}
	if event, err := syscall.WaitForSingleObject(h, 10_000); err != nil || event != syscall.WAIT_OBJECT_0 {
		t.Errorf("the sleeper %d had not ended 10s after it was terminated: %v", pid, err)
	}
}

// processImage returns the full path of a process's executable.
func processImage(h syscall.Handle) (string, error) {
	buf := make([]uint16, syscall.MAX_LONG_PATH)
	size := uint32(len(buf))
	if r, _, e := procQueryFullProcessImageNameW.Call(uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return "", e
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// runningProcesses lists the processes that could hold a TempDir, for a test
// that could not remove its own.
func runningProcesses() string {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_Process | Where-Object { $_.Name -match '^(git|sh|bash|sleep|ssh|gitsync\\.test)(\\.exe)?$' } | "+
			"Format-Table -AutoSize ProcessId,ParentProcessId,Name,CommandLine | Out-String -Width 400").CombinedOutput()
	if err != nil {
		return "listing processes failed: " + err.Error() + "\n" + string(out)
	}
	return string(out)
}
