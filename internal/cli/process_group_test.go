//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunnerCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	shellPIDFile := filepath.Join(dir, "shell.pid")
	r := &ExecRunner{Binary: writeStub(t, `echo $$ > "$1"; (sleep 5) & echo $! > "$2"; wait`)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := r.Run(ctx, shellPIDFile, childPIDFile)
		result <- err
	}()

	shellPID, childPID := waitForPIDFiles(t, shellPIDFile, childPIDFile)
	t.Cleanup(func() {
		_ = syscall.Kill(shellPID, syscall.SIGKILL)
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	})
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecRunner did not return after cancellation")
	}

	deadline := time.Now().Add(time.Second)
	for processExists(childPID) || processExists(shellPID) {
		if time.Now().After(deadline) {
			t.Fatalf("process group survived cancellation: shell=%d child=%d", shellPID, childPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPIDFiles(t *testing.T, paths ...string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pids := make([]int, 0, len(paths))
		ready := true
		for _, path := range paths {
			pid, err := readPID(path)
			if err != nil {
				ready = false
				break
			}
			pids = append(pids, pid)
		}
		if ready {
			return pids[0], pids[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID files were not created: %v", paths)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("PID file %q = %q", path, data)
	}
	return pid, nil
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
