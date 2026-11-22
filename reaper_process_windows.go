//go:build windows

package container

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureReaperProcess puts the reaper in its own process group and a
// hidden console. A shared console would deliver Ctrl+C and console-close
// events to the reaper together with the parent, killing the watchdog at the
// exact moment it is needed. CREATE_NEW_PROCESS_GROUP disables Ctrl+C for the
// group, and CREATE_NO_WINDOW holds a private console that the parent's
// console events cannot reach. Backend children inherit the hidden console,
// so no window appears for them either.
func configureReaperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}

// reaperJob is the Windows Job Object that owns one reaper and every process
// the reaper starts. A Windows console process group only routes Ctrl+C and
// cannot be terminated as a unit, and there is no portable way to walk a
// process tree by PID, so the job is the only kernel grouping that includes
// descendants created after the reaper started. The stop path can therefore
// terminate the reaper and everything it started as one tree. The native loop
// starts no backend command before EOF, so this is defense in depth for a loop
// that reports earlier: a direct-child kill alone would miss those
// descendants.
//
// The job deliberately does not set JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. That
// limit terminates every associated process when the last job handle closes,
// and the only handle lives in this process. The kernel closes it when the
// parent dies, which is exactly the moment the watchdog has to survive to
// reap. Without the limit, closing the handle leaves the reaper running and
// the job is destroyed once its last associated process exits; a stop while
// the handle is still open terminates the tree explicitly.
type reaperJob struct {
	handle windows.Handle
}

// attachReaperJob creates a job object and assigns the started reaper to it.
// It runs after Start because the standard library does not expose the process
// handle created there. That is still safe: the handle stays open until the
// sole Wait, so the PID cannot be recycled before OpenProcess resolves it. The
// gap also cannot leak a descendant, because the native reaper loop changes
// nothing until stdin reaches EOF, and the parent stops the child before it
// closes stdin.
//
// A zero job is a supported outcome. Job creation or assignment can fail, for
// example when the environment does not permit nested jobs; the reaper then
// still runs, and the stop path falls back to terminating the direct child as
// it did before the job existed.
func attachReaperJob(cmd *exec.Cmd) reaperJob {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return reaperJob{}
	}
	job := reaperJob{handle: handle}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		job.close()
		return reaperJob{}
	}
	defer func() { _ = windows.CloseHandle(process) }()
	// Children inherit the job, so everything the reaper starts after this
	// point is in it without any cooperation from the reaper.
	if err := windows.AssignProcessToJobObject(handle, process); err != nil {
		job.close()
		return reaperJob{}
	}
	return job
}

// terminate stops every process still associated with the job. It is
// TerminateProcess for the whole tree: no process can handle or postpone it.
// The caller still kills the direct child afterwards, so a failed job call
// degrades to the previous direct-child behavior.
func (j *reaperJob) terminate() {
	if j.handle == 0 {
		return
	}
	_ = windows.TerminateJobObject(j.handle, 1)
}

// close releases the handle. Because KILL_ON_JOB_CLOSE is not set, this never
// terminates an associated process, so the reaper can outlive the parent.
func (j *reaperJob) close() {
	if j.handle == 0 {
		return
	}
	_ = windows.CloseHandle(j.handle)
	j.handle = 0
}

// shellReaperAvailable reports that Windows has no /bin/sh fallback.
func shellReaperAvailable() bool { return false }

func killReaperProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
