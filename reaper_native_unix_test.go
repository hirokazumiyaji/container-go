//go:build !windows

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeReaperDeletesAppleGenerationUnderNameLock(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "delete")
	name := nativeReaperName("guarded")
	creation := strings.Repeat("b", 16)
	lockPath, err := reaperNameLockPath(name)
	if err != nil {
		t.Fatalf("reaperNameLockPath: %v", err)
	}
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation))

	entry := "+\t" + name + "\t" + creation + "\t" + lockPath + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(entry), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	got := readNativeReaperLog(t, logPath)
	want := []string{"--force " + name}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("deleted %v, want %v; stderr: %s", got, want, stderr.String())
	}
}

func TestNativeReaperLockTimeoutSkipsOnlyOneEntry(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "delete")
	cfg.lockTimeout = 300 * time.Millisecond
	creation := strings.Repeat("b", 16)

	blocked := nativeReaperName("blocked")
	blockedLock, err := reaperNameLockPath(blocked)
	if err != nil {
		t.Fatalf("reaperNameLockPath: %v", err)
	}
	held, err := openVerifiedLockFile(blockedLock)
	if err != nil {
		t.Fatalf("openVerifiedLockFile: %v", err)
	}
	if err := flockFile(int(held.Fd()), lockExclusiveNonBlocking); err != nil {
		t.Fatalf("flock: %v", err)
	}
	t.Cleanup(func() {
		_ = flockFile(int(held.Fd()), lockUnlock)
		_ = held.Close()
	})

	// Pick a name whose bucket lock is not the one this test holds.
	free := ""
	freeLock := blockedLock
	for i := 0; freeLock == blockedLock; i++ {
		free = nativeReaperName(fmt.Sprintf("free-%d", i))
		freeLock, err = reaperNameLockPath(free)
		if err != nil {
			t.Fatalf("reaperNameLockPath: %v", err)
		}
	}
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":"ctr","configuration":{"labels":{%q:%q}}}]`, creationLabel, creation))

	entries := "+\t" + blocked + "\t" + creation + "\t" + blockedLock + "\n" +
		"+\t" + free + "\t" + creation + "\t" + freeLock + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(entries), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 || lines[0] != "container-go: reaper lock timed out; skipping one entry" {
		t.Fatalf("stderr = %q, want one lock-timeout diagnostic", stderr.String())
	}
	got := readNativeReaperLog(t, logPath)
	want := []string{"--force " + free}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("deleted %v, want %v; a timed-out lock must not stop other entries", got, want)
	}
}

func TestNativeReaperSkipsSymlinkLock(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "delete")
	name := nativeReaperName("linked")
	creation := strings.Repeat("b", 16)
	// Keep the substituted path outside the shared lock namespace: a
	// symlink left on a bucket lock would break every later test.
	dir := t.TempDir()
	target := filepath.Join(dir, "target.lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "substituted.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation))

	entry := "+\t" + name + "\t" + creation + "\t" + lockPath + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(entry), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 0 {
		t.Fatalf("deleted %v, want a substituted lock path skipped", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for a skipped lock path", stderr.String())
	}
}

func TestNativeReaperSelfExecReapsPendingCreate(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("subprocess retry; no shared state under test")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	counter := filepath.Join(t.TempDir(), "inspect-count")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	t.Setenv(testNativeReaperInspectCounter, counter)
	t.Setenv(testNativeReaperInspectFailCount, "1")
	name := nativeReaperName("settling")
	creation := strings.Repeat("b", 16)
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation))
	r := newReaper(exe, "delete")
	if err := r.registerPending(name, creation); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	r.closeStdin()
	waitForNativeReaperLog(t, logPath, "--force "+name)
	waitForNativeReaperExit(t, r)
	if count := len(readNativeReaperLog(t, logPath+".inspect")); count < 2 {
		t.Fatalf("inspected %d times, want at least 2 after stdin EOF for a pending create", count)
	}
}

func TestNativeReaperReportsLockFileFailure(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "delete")
	name := nativeReaperName("bad-lock")
	creation := strings.Repeat("b", 16)
	lockPath := filepath.Join(t.TempDir(), "insecure.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation))

	entry := "+\t" + name + "\t" + creation + "\t" + lockPath + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(entry), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := stderr.String(); got != "container-go: reaper lock failed; skipping one entry\n" {
		t.Fatalf("stderr = %q, want a lock-failure diagnostic", got)
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 0 {
		t.Fatalf("deleted %v, want an unverified lock skipped", got)
	}
}

// The native loop must parse inspect output itself. Run the self-executed
// reaper with an empty PATH so a dependency on sed or awk would fail.
func TestNativeReaperDoesNotNeedExternalInspectTools(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	t.Setenv("PATH", t.TempDir())
	name := nativeReaperName("no-tools")
	creation := strings.Repeat("b", 16)
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation))
	r := newReaper(exe, "delete")
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForNativeReaperLog(t, logPath, "--force "+name)
	waitForNativeReaperExit(t, r)
}

func TestNativeReaperSelfExecAppleDelete(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	name := nativeReaperName("apple")
	creation := strings.Repeat("b", 16)
	t.Setenv(testNativeReaperInspectJSON, fmt.Sprintf(
		`[{"id":%q,"configuration":{"labels":{%q:%q}}}]`, name, creationLabel, creation))
	r := newReaper(exe, "delete")
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForNativeReaperLog(t, logPath, "--force "+name)
	waitForNativeReaperExit(t, r)
}

// TestNativeReaperKillsTimedOutBackend pins that a backend call which outlives
// its per-entry timeout does not leave the direct child running. The shell
// reaper's timer cleanup test is gone; this is the native equivalent for the
// process the reaper itself starts.
func TestNativeReaperKillsTimedOutBackend(t *testing.T) {
	cfg, _ := nativeReaperBackend(t, "rm")
	cfg.timeout = 2 * time.Second
	pidPath := filepath.Join(t.TempDir(), "backend.pid")
	t.Setenv(testNativeReaperDeletePIDFile, pidPath)
	t.Setenv(testNativeReaperDeleteSleepMS, "60000")
	id := nativeReaperID('a')
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader("+\t"+id+"\n"), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("backend did not report its PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("backend PID = %q: %v", data, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed-out backend process %d survived the reap", pid)
}
