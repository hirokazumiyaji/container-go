//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestTerminateProcessTreeUsesRetainedProcessHandle(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "timeout", "/T", "30", "/NOBREAK")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	if err := terminateProcessTree(cmd); err != nil {
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatalf("terminateProcessTree on live process = %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatal("live process was not terminated")
	}
}

func TestTerminateProcessTreeRejectsMissingProcess(t *testing.T) {
	if err := terminateProcessTree(&exec.Cmd{}); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminateProcessTree without Process = %v, want os.ErrProcessDone", err)
	}
}
