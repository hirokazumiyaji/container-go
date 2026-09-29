//go:build !windows

package container

import (
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

// A backend CLI runs its helpers in the process group the reaper created for
// the operation, exactly as docker and container do. A timed-out operation
// must therefore take the whole group down: signalling only the wrapper would
// leave the helper running, still holding the watchdog's pipe and locks.
func TestReaperTimeoutTerminatesBackendProcessTree(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = rm ]; then\n" +
		"  /bin/sh -c 'sleep 30' &\n" +
		"  echo \"$!\" > " + childPIDPath + "\n" +
		"  wait\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	uid := strings.Repeat("ab", 32)
	r := newReaper(bin, "rm")
	r.command = func() *exec.Cmd {
		return reaperCommandWithTimeouts(bin, "rm", 2, 1, 1)
	}
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	// EOF lets the child consume the registered record and enter the
	// bounded backend operation.
	r.closeStdin()

	var childPID int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(childPIDPath)
		if err == nil {
			if value, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
				childPID = value
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 1 {
		calls, _ := os.ReadFile(logPath)
		t.Fatalf("backend child pid = %d, want a spawned descendant in %s; calls=%q", childPID, childPIDPath, calls)
	}

	// The one-second bounded rm operation should now terminate the whole
	// operation, helper included.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			childPID = 0
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID != 0 {
		state, _ := exec.Command("ps", "-p", strconv.Itoa(childPID), "-o", "pid=,ppid=,pgid=,stat=,command=").CombinedOutput()
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("timed-out backend descendant %d survived: %s", childPID, state)
	}

	// The watchdog itself must finish after the bounded operation rather
	// than retaining a pipe or a lock while its child tree is cleaned up.
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("reaper did not exit after bounded backend cleanup")
		}
	}
}
