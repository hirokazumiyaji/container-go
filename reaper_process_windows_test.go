//go:build windows

package container

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// The reaper must not inherit the parent's console process group: Ctrl+C or
// closing that console would kill the watchdog together with the parent. The
// flags are observable without starting a process.
func TestConfigureReaperProcessUsesHiddenProcessGroup(t *testing.T) {
	cmd := &exec.Cmd{}
	configureReaperProcess(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("configureReaperProcess left SysProcAttr nil")
	}
	want := uint32(syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	if cmd.SysProcAttr.CreationFlags&want != want {
		t.Fatalf("CreationFlags = %#x, want %#x", cmd.SysProcAttr.CreationFlags, want)
	}
}
