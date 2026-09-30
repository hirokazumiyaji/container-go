//go:build linux

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestIssue116EscapingProcessGroupHelper(t *testing.T) {
	if os.Getenv("CONTAINERGO_GROUP_ESCAPE_HELPER") != "1" {
		return
	}
	parentGroup, err := unix.Getpgid(os.Getppid())
	if err != nil {
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	child := exec.Command("sleep", "5")
	child.Stdout = devNull
	child.Stderr = devNull
	if err := child.Start(); err != nil {
		_ = devNull.Close()
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	_ = devNull.Close()
	if err := syscall.Setpgid(0, parentGroup); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	if err := os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return
	}
	_ = child.Wait()
}

func TestExecRunnerCancellationKillsChildThatLeavesProcessGroup(t *testing.T) {
	if !processGroupTerminationSupported() {
		t.Skip("stable process identity unavailable")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escaped-child.pid")
	errorFile := filepath.Join(dir, "escaped-child.error")
	wrapper := writeStub(t, `exec env CONTAINERGO_GROUP_ESCAPE_HELPER=1 CONTAINERGO_GROUP_ESCAPE_PID="$1" CONTAINERGO_GROUP_ESCAPE_ERROR="$2" "$3" -test.run='^TestIssue116EscapingProcessGroupHelper$'`)
	r := &ExecRunner{Binary: wrapper}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := r.Run(ctx, pidFile, errorFile, os.Args[0])
		result <- err
	}()

	childPID, available := waitForSinglePID(t, pidFile, errorFile)
	if !available {
		t.Skip("process-group escape is unavailable on this host")
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecRunner did not return after escaped child cancellation")
	}
	// The child remains in the helper's former process group. A stale
	// numeric-PGID signal would kill it; the safe identity check must not.
	time.Sleep(100 * time.Millisecond)
	if !processExists(childPID) {
		t.Fatalf("former process-group member %d was killed after the direct child escaped", childPID)
	}
}

func TestExecRunnerCancellationKillsProcessGroup(t *testing.T) {
	if !processGroupTerminationSupported() {
		t.Skip("stable process identity unavailable")
	}
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

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
