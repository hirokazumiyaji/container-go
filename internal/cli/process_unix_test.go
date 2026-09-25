//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminateProcessTreeAfterWaitUsesHandleDoneState(t *testing.T) {
	cmd := exec.Command(writeStub(t, `exit 0`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := terminateProcessTree(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate after Wait = %v, want os.ErrProcessDone", err)
	}
}

func TestExecRunnerCancellationTerminatesProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	stub := writeStub(t, `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; wait "$child"`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := (&ExecRunner{Binary: stub}).Run(ctx, pidFile)
		result <- err
	}()

	childPID := waitForDescendantPID(t, pidFile)
	t.Cleanup(func() {
		if process, err := os.FindProcess(childPID); err == nil {
			_ = process.Kill()
		}
	})
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecRunner.Run did not return after cancellation")
	}
	assertStreamProcessTerminated(t, childPID)
}
