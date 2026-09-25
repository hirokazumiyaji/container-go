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

func processGroupTerminationSupported() bool { return true }

// Windows process-tree termination uses a Job Object handle rather than a
// numeric PID. The handle remains valid across child exit and cannot be
// redirected to a reused PID. Assignment intentionally happens after Start
// (see the lifecycle comment); descendants created in that small window are
// outside the job boundary. If assignment is unavailable, the caller falls
// back to the direct os.Process handle.
func configureProcessTree(*exec.Cmd) {}

type windowsProcessTree struct {
	mu      sync.Mutex
	job     windows.Handle
	process windows.Handle
	closed  bool
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
	// identifies the child and cannot have been recycled. Keep the process
	// handle so termination can distinguish an active child from a finished
	// or empty job.
	process, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	err = windows.AssignProcessToJobObject(job, process)
	if err != nil {
		_ = windows.CloseHandle(process)
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &windowsProcessTree{job: job, process: process}, nil
}

func (t *windowsProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.process == 0 {
		return terminationResult{err: os.ErrProcessDone}
	}
	active, activeErr := windowsProcessActive(t.process)
	if activeErr == nil && !active {
		// TerminateJobObject succeeds for an empty job, but that does not
		// prove that a live child was terminated.
		return terminationResult{err: os.ErrProcessDone}
	}
	if t.job != 0 {
		if err := windows.TerminateJobObject(t.job, 1); err == nil && activeErr == nil {
			return terminationResult{active: active}
		}
	}
	// Job assignment can fail, and a job can be closed concurrently with a
	// normal child exit. The direct handle remains a safe fallback.
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err == nil {
		return terminationResult{active: activeErr == nil && active}
	} else if errors.Is(err, os.ErrProcessDone) {
		return terminationResult{err: os.ErrProcessDone}
	} else {
		return terminationResult{err: err}
	}
}

// windowsProcessActive uses the process object's signaled state rather than
// GetExitCodeProcess: 259 is reserved as STILL_ACTIVE but can also be a real
// application exit code.
func windowsProcessActive(process windows.Handle) (bool, error) {
	event, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		return false, err
	}
	return event == uint32(windows.WAIT_TIMEOUT), nil
}

func (t *windowsProcessTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 && !t.closed {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
	if t.process != 0 && !t.closed {
		_ = windows.CloseHandle(t.process)
		t.process = 0
	}
	t.closed = true
}

func terminateDirectProcess(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	process, queryErr := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	active := false
	known := false
	if queryErr == nil {
		active, queryErr = windowsProcessActive(process)
		_ = windows.CloseHandle(process)
		known = queryErr == nil
	}
	killErr := cmd.Process.Kill()
	if killErr == nil {
		return terminationResult{active: known && active}
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		return terminationResult{err: os.ErrProcessDone}
	}
	return terminationResult{err: killErr}
}

// terminateProcessTree is kept as the direct-child fallback used by package
// tests and by callers that have not attached a job handle.
func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}

func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcess(cmd)
}
