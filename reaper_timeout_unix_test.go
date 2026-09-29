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

// The operation budget is deliberately longer than the stub needs to start, so
// the assertions below measure process-tree termination rather than machine
// load. The helper outlives killTimeout by orders of magnitude, so it can only
// disappear because the timeout killed it.
const (
	operationBudgetSeconds = 3
	killTimeout            = 30 * time.Second
)

func TestReaperScriptParsesWithPOSIXShell(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-n", "-c", reaperScript, "containergo-reaper")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("/bin/sh -n: %v (%s)", err, output)
	}
}

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
		"  sleep 300 &\n" +
		"  echo \"$!\" > " + childPIDPath + "\n" +
		"  wait\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	uid := strings.Repeat("ab", 32)
	// The stub has to reach its pid file before the operation budget expires,
	// so the budget must exceed the stub's startup even on a loaded machine.
	// Termination is still bounded: the helper is asserted gone within
	// killTimeout below, far under the helper's own five seconds.
	cmd := reaperCommandWithTimeouts(bin, "rm", 2, operationBudgetSeconds, 3)
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if _, err := stdin.Write([]byte("A\t" + uid + "\t\n")); err != nil {
		t.Fatalf("write reaper record: %v", err)
	}
	_ = stdin.Close()

	var childPID int
	deadline := time.Now().Add(killTimeout)
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

	// The bounded rm operation should now terminate the whole operation,
	// helper included.
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

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
		waited = true
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
		waited = true
		t.Fatal("reaper command did not exit after bounded backend cleanup")
	}
}
