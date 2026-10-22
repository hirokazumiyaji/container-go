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

func TestKillReaperProcessTerminatesProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", pidFile)
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
			child, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && child > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 0 {
		killReaperProcess(cmd, reaperProcessGroupID(cmd))
		<-exited
		t.Fatal("reaper child did not start")
	}

	killReaperProcess(cmd, reaperProcessGroupID(cmd))
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper shell did not exit")
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reaper child %d survived process-group kill", child)
}
