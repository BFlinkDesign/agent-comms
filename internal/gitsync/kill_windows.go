//go:build windows

package gitsync

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Windows has no process groups. Killing git itself is not enough: the git on
// PATH is usually Git for Windows' launcher, which starts the real git, which
// starts ssh or a remote helper, and os.Process.Kill ends only the first. So git
// runs in a job object instead, and cancelling the sync terminates the job,
// which ends every process in it.
//
// A process joins a job only after it exists, and a child it started before
// joining would stay outside. So git is created suspended, joined to the job,
// and only then resumed: it has run no code, and so started nothing, before it
// is in the job. Every process it starts afterwards joins the job with it, and
// none can leave it, because the job does not allow breakaway.
//
// The job is also created kill-on-close, and its only handle is closed once git
// has been waited for, so a child still running after git exits (an ssh left
// behind) ends too.
//
// If the job cannot be created, or git cannot be put in it (an outer job that
// forbids nesting, say), git runs as it did before job objects were used: the
// context kills git itself, and cmd.WaitDelay still returns control to the
// caller. That is counted in jobFallbacks so tests can see it happen. If git
// cannot be resumed (endpoint software that filters access to other processes,
// say), the suspended git, which has run no code, is killed and git is started
// again the same way outside any job; that is counted in resumeFallbacks too.

// Values from the Win32 documentation.
const (
	createSuspended                   = 0x00000004 // CREATE_SUSPENDED
	jobObjectExtendedLimitInformation = 9          // JOBOBJECTINFOCLASS
	jobObjectLimitKillOnJobClose      = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	processSetQuota                   = 0x0100     // PROCESS_SET_QUOTA
	processTerminate                  = 0x0001     // PROCESS_TERMINATE
	processSuspendResume              = 0x0800     // PROCESS_SUSPEND_RESUME
	killExitCode                      = 1          // the exit code os.Process.Kill uses
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtResumeProcess          = ntdll.NewProc("NtResumeProcess")
)

// createJob, assignJob, resumeGit and killGit are variables so tests can make
// them fail.
var (
	createJob = newKillOnCloseJob
	assignJob = assignToJob
	resumeGit = resumeProcess
	killGit   = (*os.Process).Kill
)

// jobFallbacks counts the git commands that ran outside a job because the job
// could not be created or joined, or git could not be resumed in it.
// resumeFallbacks counts, among those, the ones git could not be resumed for.
var jobFallbacks, resumeFallbacks atomic.Int64

// ioCounters mirrors IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// extendedLimitInformation mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION;
// basicLimitInformation is declared per architecture.
type extendedLimitInformation struct {
	BasicLimitInformation basicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// tree is one git command and the job it runs in. mu keeps cancel from running
// between putting git in the job and resuming it.
type tree struct {
	cmd *exec.Cmd
	job syscall.Handle

	mu       sync.Mutex
	inJob    bool // git is in the job, so cancelling terminates the job
	canceled bool
	closed   bool
}

// runTree runs the git command newCmd makes in a kill-on-close job and, when the
// context ends, terminates the job, so a child such as ssh cannot keep the sync
// waiting. newCmd is called again only if git has to be started a second time.
func runTree(newCmd func() *exec.Cmd) error {
	job, err := createJob()
	if err != nil {
		jobFallbacks.Add(1)
		return newCmd().Run()
	}
	cmd := newCmd()
	t := &tree{cmd: cmd, job: job}
	defer t.close()

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
	// CommandContext sets Cancel; only a command with a context is replaced.
	if cmd.Cancel != nil {
		cmd.Cancel = t.cancel
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := t.adopt(); err != nil {
		// git is still suspended, so it has run no code and started nothing:
		// kill it and run git again, outside any job, as if there were none.
		if kerr := killGit(cmd.Process); kerr != nil {
			// Wait would last as long as git stays suspended, which may be
			// forever, so return now. If git joined the job, closing the job
			// below ends it; otherwise it is left suspended, having run nothing.
			return fmt.Errorf("git could not be resumed (%v) or killed: %w", err, kerr)
		}
		_ = cmd.Wait()
		resumeFallbacks.Add(1)
		jobFallbacks.Add(1)
		return newCmd().Run()
	}
	return cmd.Wait()
}

// adopt puts the suspended git in the job, or leaves it out if that fails, and
// then lets it run.
func (t *tree) adopt() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.canceled {
		// cancel has already killed it; Wait reports the context's error.
		return nil
	}
	if err := assignJob(t.job, t.cmd.Process.Pid); err != nil {
		if err := resumeGit(t.cmd.Process.Pid); err != nil {
			return err
		}
		jobFallbacks.Add(1)
		return nil
	}
	t.inJob = true
	if err := resumeGit(t.cmd.Process.Pid); err != nil {
		// Not yet running, so cancel must kill git alone, not the job.
		t.inJob = false
		return err
	}
	return nil
}

// cancel is the command's Cancel, called once when the context ends.
func (t *tree) cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.canceled = true
	if t.inJob && !t.closed {
		r, _, e := procTerminateJobObject.Call(uintptr(t.job), killExitCode)
		if r == 0 {
			// Fall back to killing git alone rather than nothing.
			if err := t.cmd.Process.Kill(); err != nil {
				return fmt.Errorf("TerminateJobObject: %w", e)
			}
		}
		return nil
	}
	return t.cmd.Process.Kill()
}

// close closes the job's only handle, which ends anything still in it.
func (t *tree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		_ = syscall.CloseHandle(t.job)
	}
}

func newKillOnCloseJob() (syscall.Handle, error) {
	h, _, e := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateJobObjectW: %w", e)
	}
	var info extendedLimitInformation
	// Neither breakaway limit is set, so no process in the job can leave it.
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, e := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return 0, fmt.Errorf("SetInformationJobObject: %w", e)
	}
	return syscall.Handle(h), nil
}

// assignToJob puts a process in the job. The process is opened by its id, which
// cannot have been reused, even if something outside fleetd has killed it: the
// caller's exec.Cmd holds a handle to the process until Wait, and Windows does
// not give a process's id to another while a handle to it is open.
func assignToJob(job syscall.Handle, pid int) error {
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)
	r, _, e := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h))
	if r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", e)
	}
	return nil
}

// resumeProcess resumes a process created suspended. It resumes the process as
// a whole, through a handle opened by id (the id cannot have been reused, for
// the reason assignToJob gives), so it needs no snapshot of the system's threads. Each thread's
// suspend count drops by one: if something else suspended git as well, git runs
// once that has resumed it too.
func resumeProcess(pid int) error {
	if err := procNtResumeProcess.Find(); err != nil {
		return err
	}
	h, err := syscall.OpenProcess(processSuspendResume, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)
	status, _, _ := procNtResumeProcess.Call(uintptr(h))
	if int32(uint32(status)) < 0 { // !NT_SUCCESS
		return fmt.Errorf("NtResumeProcess: NTSTATUS %#08x", uint32(status))
	}
	return nil
}
