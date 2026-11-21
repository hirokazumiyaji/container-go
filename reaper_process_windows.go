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

// shellReaperAvailable reports that Windows has no /bin/sh fallback.
func shellReaperAvailable() bool { return false }

func killReaperProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
