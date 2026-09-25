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

func TestReaperDoesNotStageInspectOutput(t *testing.T) {
	dir := t.TempDir()
	tmpDir := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpDir)
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "container")
	const secret = "reaper-inspect-secret"
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  printf '{\\\"password\\\":\\\"" + secret + "\\\",\\n  \\\"com.github.hirokazumiyaji.container-go.creation\\\":\\\"0123456789abcdef\\\"}\\n'\n" +
		"  sleep 1\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.register("secret-ctr", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(tmpDir)
		for _, entry := range entries {
			data, _ := os.ReadFile(filepath.Join(tmpDir, entry.Name()))
			if strings.Contains(string(data), secret) {
				t.Fatalf("inspect output was staged in %s", filepath.Join(tmpDir, entry.Name()))
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForLogLines(t, logPath, "delete --force secret-ctr")
}

func TestReaperReplayWaitsForOldDescendantGroup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	oldPIDPath := filepath.Join(dir, "old.pid")
	startedPath := filepath.Join(dir, "old.started")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = delete ] && [ \"$3\" = old ]; then\n" +
		"  if [ ! -e " + startedPath + " ]; then\n" +
		"    : > " + startedPath + "\n" +
		"    sleep 30 &\n" +
		"    echo $! > " + oldPIDPath + "\n" +
		"  fi\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = delete ] && [ \"$3\" = new ]; then\n" +
		"  if kill -0 \"$(cat " + oldPIDPath + ")\" 2>/dev/null; then echo OVERLAP >> " + logPath + "; fi\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.timeoutSeconds = 2
	if err := r.register("old", ""); err != nil {
		t.Fatalf("register old: %v", err)
	}
	r.mu.Lock()
	oldCmd := r.cmd
	stdin := r.stdin
	r.mu.Unlock()
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitForReaperPIDFile(t, oldPIDPath)
	waitForReaperReplacement(t, r, oldCmd)
	if err := r.register("new", ""); err != nil {
		t.Fatalf("register new: %v", err)
	}
	// The reaper intentionally buffers its replay set until EOF. Closing
	// stdin here makes the replacement process both retained entries.
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force new")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "OVERLAP") {
		t.Fatalf("replacement overlapped old descendant: %s", data)
	}
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
