//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStreamEOFBoundsDescendantPipeRetentionAfterChildExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	r := &ExecRunner{Binary: writeStub(t, `(sleep 5) & echo $! > "$3"; printf 'parent\\n'`)}
	stream, err := r.Stream(context.Background(), "logs", "x", pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	descendantPID, available := waitForSinglePID(t, pidFile, filepath.Join(filepath.Dir(pidFile), "missing.error"))
	if !available {
		t.Fatal("descendant PID file was not created")
	}
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })

	started := time.Now()
	data, readErr := io.ReadAll(stream)
	if readErr != nil {
		t.Fatalf("ReadAll: %v", readErr)
	}
	if !strings.Contains(string(data), "parent") {
		t.Fatalf("stream data = %q, want parent output", data)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stream EOF took %v, want bounded drain after direct child exit", elapsed)
	}
}

func TestIssue116EscapingProcessGroupHelper(t *testing.T) {
	if os.Getenv("CONTAINERGO_GROUP_ESCAPE_HELPER") != "1" {
		return
	}
	parentGroup, err := syscall.Getpgid(os.Getppid())
	if err != nil {
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	if err := syscall.Setpgid(0, parentGroup); err != nil {
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		_ = os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_ERROR"), []byte(err.Error()), 0o600)
		return
	}
	if err := os.WriteFile(os.Getenv("CONTAINERGO_GROUP_ESCAPE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		return
	}
	_ = child.Wait()
}

func TestExecRunnerCancellationKillsChildThatLeavesProcessGroup(t *testing.T) {
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
	// The helper's detached sleep is intentionally outside the direct-child
	// guarantee; cleanup above removes it after the prompt-return assertion.
}

func waitForSinglePID(t *testing.T, path, errorPath string) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if pid, err := readPID(path); err == nil {
			return pid, true
		}
		if _, err := os.Stat(errorPath); err == nil {
			return 0, false
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID file %q was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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
