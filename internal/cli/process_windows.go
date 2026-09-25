//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process-tree termination uses a Job Object handle rather than a
// numeric PID. The handle remains valid across child exit and cannot be
// redirected to a reused PID. If the process cannot be assigned to a job
// (for example, because the caller is already inside a restrictive job), the
// caller falls back to the direct os.Process handle.
func configureProcessTree(*exec.Cmd) {}

type windowsProcessTree struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func newProcessTree(cmd *exec.Cmd) (processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	// The lifecycle has not called Wait yet, so this numeric PID still
	// identifies the child and cannot have been recycled. The resulting
	// job handle, unlike the PID, remains valid for later termination.
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	err = windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &windowsProcessTree{job: job}, nil
}

func (t *windowsProcessTree) terminate(cmd *exec.Cmd) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 && !t.closed {
		if err := windows.TerminateJobObject(t.job, 1); err == nil {
			return nil
		}
	}
	// Job assignment can fail, and a job can be closed concurrently with a
	// normal child exit. The direct handle remains a safe fallback.
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := cmd.Process.Kill(); err == nil {
		return nil
	} else if errors.Is(err, os.ErrProcessDone) {
		return os.ErrProcessDone
	} else {
		return err
	}
}

func (t *windowsProcessTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 && !t.closed {
		_ = windows.CloseHandle(t.job)
		t.job = 0
		t.closed = true
	}
}

// terminateProcessTree is kept as the direct-child fallback used by package
// tests and by callers that have not attached a job handle.
func terminateProcessTree(cmd *exec.Cmd) error {
	return directProcessTree{}.terminate(cmd)
}
