//go:build !windows

package container

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReaperReplayFailureStopsAndReapsChildBeforeRetry(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	pidPath := filepath.Join(t.TempDir(), "blackhole.pid")
	r := newReaper(bin, "delete")
	// The registration path, rather than the supervisor, owns the retry in
	// this test. Make the first child accept its pipe but never read it, so
	// replay fails after a process has definitely been spawned.
	r.supervise = false
	r.writeTimeout = 50 * time.Millisecond
	spawns := 0
	r.command = func() *exec.Cmd {
		spawns++
		if spawns == 1 {
			return exec.Command("/bin/sh", "-c", fmt.Sprintf("echo $$ > %q; sleep 5", pidPath))
		}
		return reaperCommandWithTimeouts(bin, "delete", 2, 2, 2)
	}

	// Let the first registration establish the child, then fill its pipe
	// without reading it. The next registration write is the replay failure
	// that exercises the retry path.
	if err := r.register("replay-failure", "0123456789abcdef"); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	r.mu.Lock()
	entry := r.entries[0]
	for i := 0; i < 1024; i++ {
		if err := r.writeRecordLocked(entry.record(reaperStateActive)); err != nil {
			break
		}
	}
	r.mu.Unlock()
	if err := r.register("replay-failure", "0123456789abcdef"); err != nil {
		t.Fatalf("register after replay failure: %v", err)
	}

	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if value, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
				pid = value
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatalf("blackhole child pid = %d, want a live first spawn recorded in %s", pid, pidPath)
	}

	// Replay must not leave the failed child (or its sleep descendant) alive
	// while the replacement is already running.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			pid = 0
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid != 0 {
		state, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid=,ppid=,pgid=,stat=,command=").CombinedOutput()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("failed replay child %d was not terminated and reaped: %s", pid, state)
	}

	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force replay-failure")
}
