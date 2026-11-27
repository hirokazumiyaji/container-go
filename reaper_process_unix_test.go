//go:build !windows

package container

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

// startReaperGroup starts a shell group with one long-lived member and
// returns the group leader's command, that member's PID, and the exited
// channel owned by the sole Wait call.
func startReaperGroup(t *testing.T) (*exec.Cmd, int, <-chan error) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", pidFile)
	configureReaperProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start reaper: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var child int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if child > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 0 {
		killReaperProcess(cmd)
		<-exited
		t.Fatal("reaper child did not start")
	}
	return cmd, child, exited
}

func TestKillReaperProcessTerminatesProcessGroup(t *testing.T) {
	cmd, child, exited := startReaperGroup(t)

	killReaperProcess(cmd)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper shell did not exit")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reaper child %d survived process-group kill", child)
}

// TestKillReaperProcessRequiresStoppedBarrier is the regression for the
// asynchronous SIGSTOP window: when the stopped state cannot be observed,
// the numeric group must not be signaled. A group member that survived the
// direct kill proves no group signal was issued.
func TestKillReaperProcessRequiresStoppedBarrier(t *testing.T) {
	cmd, child, exited := startReaperGroup(t)

	killReaperProcessWithObserver(cmd, func(*os.Process) (bool, error) {
		return false, nil
	})
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper shell did not exit after the direct kill")
	}

	// Give a (buggy) numeric group signal time to reach the members before
	// checking whether the barrier withheld it.
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(child, 0); err != nil {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("group member %d was signaled without an observed stopped barrier: %v", child, err)
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group member %d did not exit after cleanup", child)
}
