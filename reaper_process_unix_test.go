//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReaperKillsBackendDescendantsBeforeExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	childPIDPath := filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = delete ]; then\n" +
		"  sleep 30 &\n" +
		"  echo $! > " + childPIDPath + "\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.timeoutSeconds = 2
	if err := r.register("tree", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	pid := waitForReaperPIDFile(t, childPIDPath)
	waitForReaperProcessGone(t, pid)
}

func TestReaperTimeoutKillsDescendantsBeforeLaterEntries(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	childPIDPath := filepath.Join(dir, "first.pid")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = delete ] && [ \"$3\" = first ]; then\n" +
		"  sleep 30 &\n" +
		"  echo $! > " + childPIDPath + "\n" +
		"  while :; do sleep 1; done\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.timeoutSeconds = 1
	for _, id := range []string{"first", "later"} {
		if err := r.register(id, ""); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	r.closeStdin()
	pid := waitForReaperPIDFile(t, childPIDPath)
	waitForReaperProcessGone(t, pid)
	waitForLogLines(t, logPath, "delete --force later")
}

func TestReaperRespawnWaitsForOldDescendants(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	oldPIDPath := filepath.Join(dir, "old.pid")
	releasePath := filepath.Join(dir, "release")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = delete ] && [ \"$3\" = old ]; then\n" +
		"  sleep 30 &\n" +
		"  child=$!\n" +
		"  echo \"$child\" > " + oldPIDPath + "\n" +
		"  while [ ! -e " + releasePath + " ]; do sleep 1; done\n" +
		"  kill \"$child\" 2>/dev/null || true\n" +
		"  wait \"$child\" 2>/dev/null || true\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.timeoutSeconds = 5
	if err := r.register("old", ""); err != nil {
		t.Fatalf("register old: %v", err)
	}

	// Feed EOF without marking the reaper intentionally closed, modeling a
	// parent that died while the backend delete was still in progress.
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	pid := waitForReaperPIDFile(t, oldPIDPath)
	r.killForTest()
	waitForReaperProcessGone(t, pid)
	_ = os.WriteFile(releasePath, nil, 0o600)
	time.Sleep(100 * time.Millisecond)

	if err := r.register("new", ""); err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force new")
}

func waitForReaperPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for PID file %s", path)
	return 0
}

func waitForReaperProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived reaper shutdown", pid)
}
