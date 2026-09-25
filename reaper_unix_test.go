//go:build darwin || linux

package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func reaperTestShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func reaperTestSleepPIDs(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var pids []int
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		pids = nil
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
		if len(pids) >= 2 {
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	t.Fatalf("timed out waiting for timeout helper PIDs in %s", path)
	return nil
}

func reaperTestProcessAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func waitForReaperTestProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !reaperTestProcessAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("timeout helper process %d survived reaper cleanup", pid)
}

func TestReaperFastCallsReapTimeoutHelpers(t *testing.T) {
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls.log")
	sleepPIDsPath := filepath.Join(dir, "sleep-pids")
	binPath := filepath.Join(dir, "container")
	sleepPath := filepath.Join(dir, "sleep")
	creation := "0123456789abcdef"

	backendScript := "#!/bin/sh\n" +
		"/bin/sleep 0.05\n" +
		"printf '%s\\n' \"$*\" >> " + reaperTestShellQuote(callsPath) + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  printf '  \"" + creationLabel + "\": \"" + creation + "\"\\n'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(backendScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake sleeper records the exact process that the timeout helper
	// must terminate, then remains alive long enough to expose an orphan.
	sleepScript := "#!/bin/sh\n" +
		"printf '%s\\n' \"$$\" >> " + reaperTestShellQuote(sleepPIDsPath) + "\n" +
		"exec /bin/sleep 60\n"
	if err := os.WriteFile(sleepPath, []byte(sleepScript), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.command = func() *exec.Cmd {
		cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", binPath, "delete", breQuote(creationLabel))
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		return cmd
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	if err := r.register("guarded", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, callsPath, "inspect guarded", "delete --force guarded")
	waitForReaperExit(t, r)

	pids := reaperTestSleepPIDs(t, sleepPIDsPath)
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if len(pids) < 2 {
		t.Fatalf("timeout helper PIDs = %v, want inspect and delete helpers", pids)
	}
	for _, pid := range pids {
		waitForReaperTestProcessGone(t, pid)
	}
}
