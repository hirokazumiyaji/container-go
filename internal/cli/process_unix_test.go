//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTerminateProcessTreeDoesNotClaimExitedChild(t *testing.T) {
	cmd := exec.Command(writeStub(t, `exit 0`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Leave the child unreaped long enough to exercise the exited/zombie
	// path. The sole lifecycle Wait below remains the only reap.
	if !waitForProcessExitNoReap(cmd) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skip("could not observe the child exit without reaping it")
	}
	result := terminateProcessTreeResult(cmd)
	if result.active {
		t.Fatalf("termination result = %+v, exited child must not be active", result)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait after termination probe: %v", err)
	}
}

func waitForProcessExitNoReap(cmd *exec.Cmd) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := cmd.Process.Signal(syscall.Signal(0))
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

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

func TestExecRunnerCancellationReapsDirectChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	stub := writeStub(t, `printf '%s\n' "$$" > "$1"; exec sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := (&ExecRunner{Binary: stub}).Run(ctx, pidFile)
		result <- err
	}()

	childPID := waitForPIDFile(t, pidFile)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecRunner.Run did not return after cancellation")
	}
	if state, err := processState(childPID); err == nil && strings.TrimSpace(state) != "" {
		t.Fatalf("direct child state = %q, want reaped", state)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file was not written: %s", path)
	return 0
}

func processState(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}
