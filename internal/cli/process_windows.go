//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

// configureProcessTree is intentionally a no-op on Windows. The standard
// library does not expose a Job Object handle, so this package retains the
// direct process handle for evidence-aware termination. Detached or
// reparented descendants remain outside that guarantee.
func configureProcessTree(*exec.Cmd) {}

type windowsProcessTree struct {
	process windows.Handle
}

// newProcessTree is called after Start and before the sole Wait. The handle
// opened here is retained for all later cancellation attempts; no later path
// looks up the process by its numeric PID.
func newProcessTree(cmd *exec.Cmd) (processTree, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, os.ErrProcessDone
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return nil, err
	}
	return &windowsProcessTree{process: process}, nil
}

func (t *windowsProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	if t == nil || t.process == 0 {
		return terminationResult{err: os.ErrProcessDone}
	}
	active, activeErr := windowsProcessActive(t.process)
	if activeErr == nil && !active {
		return terminationResult{err: os.ErrProcessDone}
	}
	terminateErr := windows.TerminateProcess(t.process, 1)
	if terminateErr == nil {
		return terminationResult{active: true, syntheticExit: true}
	}
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: terminateErr}
	}
	if killErr := cmd.Process.Kill(); killErr == nil {
		if activeErr != nil {
			terminateErr = errors.Join(terminateErr, activeErr)
		}
		return terminationResult{active: true, syntheticExit: true, err: terminateErr}
	} else if errors.Is(killErr, os.ErrProcessDone) {
		return terminationResult{err: errors.Join(terminateErr, killErr)}
	} else {
		return terminationResult{err: errors.Join(terminateErr, killErr)}
	}
}

func (t *windowsProcessTree) close() {
	if t != nil && t.process != 0 {
		_ = windows.CloseHandle(t.process)
		t.process = 0
	}
}

func windowsProcessActive(process windows.Handle) (bool, error) {
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
		return false, err
	}
	return exitCode == windowsStillActive, nil
}

func terminateDirectProcessResult(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	return terminationResult{active: true, syntheticExit: true}
}

func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcessResult(cmd)
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}
