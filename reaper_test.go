package container

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func reaperShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// writeReaperStub creates a fake `container` binary that logs its argv.
func writeReaperStub(t *testing.T) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binPath = filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, logPath
}

func waitForLogLines(t *testing.T, path string, wants ...string) {
	t.Helper()
	waitForReaperLogLinesWithin(t, path, 5*time.Second, wants...)
}

func waitForReaperLogLinesWithin(t *testing.T, path string, timeout time.Duration, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		found := true
		for _, w := range wants {
			if !strings.Contains(string(data), w) {
				found = false
				break
			}
		}
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log %s = %q, want all of %q", path, data, wants)
}

func TestReaperDeletesRegisteredContainersOnEOF(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("ctr-one", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register("ctr-two", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Closing stdin is what the reaper sees when the parent process
	// dies, however it dies.
	closeReaperForTest(t, r)

	waitForLogLines(t, logPath, "delete --force ctr-one", "delete --force ctr-two")
}

func TestReaperRejectsInvalidID(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer closeReaperForTest(t, r)

	for _, id := range []string{"", "bad id", "a;b", "x\ny", "-leading"} {
		if err := r.register(id, ""); err == nil {
			t.Errorf("register(%q): want error", id)
		}
	}
	if err := r.register("ctr-one", "not-hex"); err == nil {
		t.Error("register bad creation: want error")
	}
}

func TestReaperRespawnsAndReRegisters(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("before-crash", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Kill the reaper child; killing closes its stdin read side, so it
	// reaps what it knows, then the next register must respawn it.
	r.killForTest()

	if err := r.register("after-crash", ""); err != nil {
		t.Fatalf("register after crash: %v", err)
	}
	closeReaperForTest(t, r)

	waitForLogLines(t, logPath, "delete --force before-crash", "delete --force after-crash")
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func readReaperPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("PID file %s = %q: %v", path, data, err)
	}
	return pid
}

func waitForReaperProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(out))
		if err != nil || state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d survived reaper cleanup", pid)
}

func closeReaperForTest(t *testing.T, r *reaper) {
	t.Helper()
	r.closeStdin()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not exit after stdin close")
	}
}

func assertReaperTreeHasNoSecret(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular entry %s (%s)", path, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			return fmt.Errorf("secret remained in reaper staging file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReaperSIGKILLDoesNotLeaveInspectSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the watchdog reaper requires a POSIX shell")
	}

	// Keep the secret-bearing fake binary outside the tree scanned below.
	// TMPDIR is changed only after both fixture directories exist.
	stagingDir := t.TempDir()
	workDir := t.TempDir()
	t.Setenv("TMPDIR", stagingDir)

	started := filepath.Join(workDir, "inspect-started")
	release := filepath.Join(workDir, "release-inspect")
	childPIDPath := filepath.Join(workDir, "inspect-child.pid")
	binPath := filepath.Join(workDir, "container")
	const secret = "reaper-secret-4f8c2a"

	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"inspect\" ]; then\n" +
		"  echo '  \"environment\": [\"PASSWORD=" + secret + "\"]'\n" +
		"  echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'\n" +
		"  : > " + reaperShellQuote(started) + "\n" +
		"  sh -c 'sleep 30' &\n" +
		"  echo \"$!\" > " + reaperShellQuote(childPIDPath) + "\n" +
		"  while [ ! -e " + reaperShellQuote(release) + " ]; do sleep 1; done\n" +
		"  wait\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	childPID := 0
	reaperKilled := false
	t.Cleanup(func() {
		if !reaperKilled {
			r.killForTest()
		}
		_ = os.WriteFile(release, nil, 0o600)
		if childPID > 0 {
			waitForReaperProcessGone(t, childPID)
		}
	})

	if err := r.register("guarded", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForPath(t, started)
	waitForPath(t, childPIDPath)
	childPID = readReaperPID(t, childPIDPath)
	r.killForTest()
	reaperKilled = true
	waitForReaperProcessGone(t, childPID)

	assertReaperTreeHasNoSecret(t, stagingDir, secret)
}

func TestReaperHungInspectDoesNotBlockLaterEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the watchdog reaper requires a POSIX shell")
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	started := filepath.Join(dir, "first-started")
	release := filepath.Join(dir, "release-first")
	childPIDPath := filepath.Join(dir, "first-child.pid")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + reaperShellQuote(logPath) + "\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = first ]; then\n" +
		"  : > " + reaperShellQuote(started) + "\n" +
		"  sh -c 'sleep 30' &\n" +
		"  echo \"$!\" > " + reaperShellQuote(childPIDPath) + "\n" +
		"  while [ ! -e " + reaperShellQuote(release) + " ]; do sleep 1; done\n" +
		"  wait\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = later ]; then\n" +
		"  echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	var childPID int
	reaperStopped := false
	t.Cleanup(func() {
		if !reaperStopped {
			r.killForTest()
		}
		_ = os.WriteFile(release, nil, 0o600)
		if childPID > 0 {
			waitForReaperProcessGone(t, childPID)
		}
	})

	r.timeoutSeconds = 1
	if err := r.register("first", "0123456789abcdef"); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := r.register("later", "0123456789abcdef"); err != nil {
		t.Fatalf("register later: %v", err)
	}
	closeReaperForTest(t, r)
	reaperStopped = true

	waitForPath(t, started)
	waitForPath(t, childPIDPath)
	childPID = readReaperPID(t, childPIDPath)
	waitForReaperLogLinesWithin(t, logPath, 8*time.Second,
		"inspect first", "inspect later", "delete --force later")
	waitForReaperProcessGone(t, childPID)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force first") {
		t.Fatalf("timed-out first entry was deleted: %q", data)
	}
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, `timeout="${4:-30}"`) ||
		!strings.Contains(reaperScript, `sleep "$timeout"`) ||
		!strings.Contains(reaperScript, "kill -9") {
		t.Error("reaper script must bound each backend call with sleep/kill (no timeout(1))")
	}
	if !strings.Contains(reaperScript, "run_with_timeout process_entry") {
		t.Error("reaper must put the complete entry pipeline behind its timeout")
	}
	// The creation label must be read as a structural JSON field, anchored
	// at line start on the quoted key, and compared for exact equality.
	if !strings.Contains(reaperScript, `s/^[[:space:]]*\"$key\"[[:space:]]*:`) {
		t.Error("reaper script must anchor the creation label match on the quoted key")
	}
	if !strings.Contains(reaperScript, `[ "$got" = "$creation" ] || return 0`) {
		t.Error("reaper script must compare the extracted generation exactly")
	}
}

func TestBreQuoteEscapesLabelKey(t *testing.T) {
	if got := breQuote("com.github.x-y"); got != `com\.github\.x-y` {
		t.Errorf("breQuote = %q", got)
	}
}

func TestReaperSpawnFailuresResetOnSuccess(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.spawnFailures = 2
	if err := r.register("ok", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	closeReaperForTest(t, r)
	if r.spawnFailures != 0 {
		t.Errorf("spawnFailures = %d, want 0 after success (consecutive counting)", r.spawnFailures)
	}
}

func TestReaperRegisterWithCreationValidation(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer closeReaperForTest(t, r)
	if err := r.register("good-id", "0123456789abcdef"); err != nil {
		t.Errorf("valid creation rejected: %v", err)
	}
	if err := r.register("good-id", "not-hex"); err == nil {
		t.Error("invalid creation accepted")
	}
}

func TestReaperGuardsDeleteByCreation(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/container"
	// Stub: inspect prints the creation it was told to know; delete is logged.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("guarded", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register("stale", "ffffffffffffffff"); err != nil {
		t.Fatalf("register: %v", err)
	}
	closeReaperForTest(t, r)
	waitForLogLines(t, logPath, "delete --force guarded")
	// Stale generation must not be deleted; poll briefly to confirm absence.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force stale") {
			t.Fatal("stale generation was deleted; want guard to skip it")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReaperRejectsLabelValueContainingAssociation(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/container"
	oldCreation := "0123456789abcdef"
	// The live creation label differs; the old generation appears only
	// inside user label values, in both key=value and (JSON-escaped)
	// quoted forms, and under a look-alike key. None is the field.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then\n" +
		"  echo '  \"" + creationLabel + "\": \"ffffffffffffffff\",'\n" +
		"  echo '  \"user.a\": \"" + creationLabel + "=" + oldCreation + "\",'\n" +
		"  echo '  \"user.b\": \"\\\"" + creationLabel + "\\\": \\\"" + oldCreation + "\\\"\",'\n" +
		"  echo '  \"" + strings.ReplaceAll(creationLabel, ".", "-") + "\": \"" + oldCreation + "\",'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("ctr", oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}
	closeReaperForTest(t, r)
	waitForLogLines(t, logPath, "inspect ctr")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force ctr") {
			t.Fatalf("reaper deleted on label value collision: %q", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReaperDeletesByImmutableID(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/docker"
	creation := "0123456789abcdef"
	uid := strings.Repeat("ab", 32)
	// Docker-style inspect: the generation matches and an immutable Id is
	// present, so the delete must target the Id rather than the name.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then\n" +
		"  echo '    \"Id\": \"" + uid + "\",'\n" +
		"  echo '      \"" + creationLabel + "\": \"" + creation + "\"'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "rm")
	if err := r.register("ctr", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	closeReaperForTest(t, r)
	waitForLogLines(t, logPath, "rm --force "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "rm --force ctr") {
		t.Fatalf("reaper deleted by name despite an immutable Id: %q", data)
	}
}
