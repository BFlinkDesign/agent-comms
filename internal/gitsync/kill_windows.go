//go:build windows

package gitsync

import (
	"fmt"
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
// caller. That is counted in jobFallbacks so tests can see it happen.

// Values from the Win32 documentation.
const (
	createSuspended                   = 0x00000004 // CREATE_SUSPENDED
	jobObjectExtendedLimitInformation = 9          // JOBOBJECTINFOCLASS
	jobObjectLimitKillOnJobClose      = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	processSetQuota                   = 0x0100     // PROCESS_SET_QUOTA
	processTerminate                  = 0x0001     // PROCESS_TERMINATE
	threadSuspendResume               = 0x0002     // THREAD_SUSPEND_RESUME
	th32csSnapThread                  = 0x00000004 // TH32CS_SNAPTHREAD
	resumeThreadFailed                = 0xFFFFFFFF // (DWORD)-1
	killExitCode                      = 1          // the exit code os.Process.Kill uses
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First            = kernel32.NewProc("Thread32First")
	procThread32Next             = kernel32.NewProc("Thread32Next")
	procOpenThread               = kernel32.NewProc("OpenThread")
	procResumeThread             = kernel32.NewProc("ResumeThread")
)

// createJob and assignJob are variables so tests can make them fail.
var (
	createJob = newKillOnCloseJob
	assignJob = assignToJob
)

// jobFallbacks counts the git commands that ran outside a job because the job
// could not be created or joined.
var jobFallbacks atomic.Int64

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

// threadEntry32 mirrors THREADENTRY32.
type threadEntry32 struct {
	Size           uint32
	CntUsage       uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

var _ [0]struct{} = [unsafe.Sizeof(threadEntry32{}) - 28]struct{}{}

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

// runTree runs git in a kill-on-close job and, when the context ends, terminates
// the job, so a child such as ssh cannot keep the sync waiting.
func runTree(cmd *exec.Cmd) error {
	job, err := createJob()
	if err != nil {
		jobFallbacks.Add(1)
		return cmd.Run()
	}
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
		// git is still suspended, so it has started nothing that could outlive it.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
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
		jobFallbacks.Add(1)
	} else {
		t.inJob = true
	}
	return resumeProcess(uint32(t.cmd.Process.Pid))
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
// cannot have been reused: it is suspended, so it cannot exit on its own, and
// the only thing that kills it, cancel, waits for the caller's lock.
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

// resumeProcess resumes the suspended primary thread of a process. os/exec
// closes the thread handle CreateProcess returns, so the thread is found in a
// snapshot of the system's threads and opened again.
func resumeProcess(pid uint32) error {
	snap, _, e := procCreateToolhelp32Snapshot.Call(th32csSnapThread, 0)
	if syscall.Handle(snap) == syscall.InvalidHandle {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", e)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	te := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	r, _, e := procThread32First.Call(snap, uintptr(unsafe.Pointer(&te)))
	if r == 0 {
		return fmt.Errorf("Thread32First: %w", e)
	}
	resumed := 0
	for {
		if te.OwnerProcessID == pid {
			th, _, e := procOpenThread.Call(threadSuspendResume, 0, uintptr(te.ThreadID))
			if th == 0 {
				return fmt.Errorf("OpenThread: %w", e)
			}
			prev, _, e := procResumeThread.Call(th)
			_ = syscall.CloseHandle(syscall.Handle(th))
			switch {
			case uint32(prev) == resumeThreadFailed:
				return fmt.Errorf("ResumeThread: %w", e)
			case prev == 1:
				resumed++
			case prev > 1:
				return fmt.Errorf("gitsync: git's thread %d is still suspended", te.ThreadID)
			}
		}
		te.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		r, _, e = procThread32Next.Call(snap, uintptr(unsafe.Pointer(&te)))
		if r == 0 {
			if e == syscall.ERROR_NO_MORE_FILES {
				break
			}
			return fmt.Errorf("Thread32Next: %w", e)
		}
	}
	if resumed == 0 {
		return fmt.Errorf("gitsync: no suspended thread found for git (pid %d)", pid)
	}
	return nil
}
