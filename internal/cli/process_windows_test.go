//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestTerminateProcessTreeUsesRetainedProcessHandle(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := terminateProcessTree(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminateProcessTree after Wait = %v, want os.ErrProcessDone", err)
	}
}

func TestTerminateProcessTreeRejectsMissingProcess(t *testing.T) {
	if err := terminateProcessTree(&exec.Cmd{}); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminateProcessTree without Process = %v, want os.ErrProcessDone", err)
	}
}
