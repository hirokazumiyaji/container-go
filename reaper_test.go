//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func reaperShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

type reaperExternalRunner struct {
	inner  cli.Runner
	binary string
	id     string
}

func (r *reaperExternalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "run" {
		return []byte(r.id + "\n"), nil, nil
	}
	return r.inner.Run(ctx, args...)
}

func (r *reaperExternalRunner) External() bool         { return true }
func (r *reaperExternalRunner) ExternalBinary() string { return r.binary }

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

func testReaperHelpers(t *testing.T, overrides reaperHelperPaths) reaperHelperPaths {
	t.Helper()
	base, err := trustedReaperHelpers()
	if err != nil {
		t.Fatal(err)
	}
	if overrides.awk != "" {
		base.awk = overrides.awk
	}
	if overrides.pgrep != "" {
		base.pgrep = overrides.pgrep
	}
	if overrides.ps != "" {
		base.ps = overrides.ps
	}
	if overrides.rm != "" {
		base.rm = overrides.rm
	}
	if overrides.sleep != "" {
		base.sleep = overrides.sleep
	}
	return base
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

func TestReaperAcceptsFullDockerID(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")

	id := strings.Repeat("ab", 32)
	if err := r.register(id, ""); err != nil {
		t.Fatalf("register full Docker ID: %v", err)
	}
	closeReaperForTest(t, r)
	waitForLogLines(t, logPath, "rm --force "+id)
}

func TestRegisterWithGlobalReaperRecordsError(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "docker")
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, binary)
		globalReapersMu.Unlock()
	})

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	if err := registerWithGlobalReaper(binary, "rm", "bad id", ""); err == nil {
		t.Fatal("registration unexpectedly succeeded")
	}
	if got := logs.String(); !strings.Contains(got, "reaper registration failed") || !strings.Contains(got, "invalid container id") {
		t.Fatalf("registration log = %q, want validation error", got)
	}
	if err := registerWithGlobalReaper(binary, "rm", "ctr-one", ""); err == nil {
		t.Fatal("generationless Docker name registration unexpectedly succeeded")
	}
	if got := logs.String(); !strings.Contains(got, "no generation") {
		t.Fatalf("registration log = %q, want generationless Docker rejection", got)
	}
}

func TestRunRegistersFullDockerIDThroughExternalPath(t *testing.T) {
	bin, _ := writeReaperStub(t)
	inner := newTestRunner()
	inner.imagePresent = true
	runner := &reaperExternalRunner{
		inner:  inner,
		binary: bin,
		id:     strings.Repeat("cd", 32),
	}

	globalReapersMu.Lock()
	delete(globalReapers, bin)
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		r := globalReapers[bin]
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		if r == nil {
			return
		}
		r.closeStdin()
		r.mu.Lock()
		exited := r.exited
		r.mu.Unlock()
		if exited != nil {
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				t.Error("external-path reaper did not exit")
			}
		}
	})

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("external-ctr"),
		WithPullPolicy(PullNever),
		withRunner(runner),
		withEngine(dockerEngine{}),
	); err != nil {
		t.Fatalf("Run: %v", err)
	}
	globalReapersMu.Lock()
	r := globalReapers[bin]
	globalReapersMu.Unlock()
	if r == nil {
		t.Fatal("external Run did not create a reaper")
	}
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || entries[0].id != runner.id || entries[0].creation != "" {
		t.Fatalf("registered entries = %+v, want full Docker ID without generation", entries)
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

func TestReaperSetsidHelperProcess(t *testing.T) {
	if os.Getenv("CONTAINERGO_REAPER_SETSID_HELPER") != "1" {
		return
	}
	child := exec.Command("/bin/sh", "-c", "sleep 30")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("CONTAINERGO_REAPER_SETSID_PID_PATH"); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(30 * time.Second)
}

func TestReaperShellSnapshotsNestedSetsidBeforeKilling(t *testing.T) {
	dir := t.TempDir()
	helperPIDPath := filepath.Join(dir, "helper.pid")
	nestedPIDPath := filepath.Join(dir, "nested-child.pid")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + reaperShellQuote(filepath.Join(dir, "calls.log")) + "\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = nested ]; then\n" +
		"  CONTAINERGO_REAPER_SETSID_HELPER=1 CONTAINERGO_REAPER_SETSID_PID_PATH=" + reaperShellQuote(nestedPIDPath) + " " + reaperShellQuote(os.Args[0]) + " -test.run=^TestReaperSetsidHelperProcess$ &\n" +
		"  echo \"$!\" > " + reaperShellQuote(helperPIDPath) + "\n" +
		"  while :; do sleep 1; done\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	r.timeoutSeconds = 1
	if err := r.register("nested", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForPath(t, helperPIDPath)
	waitForPath(t, nestedPIDPath)
	nestedPID := readReaperPID(t, nestedPIDPath)
	closeReaperForTest(t, r)
	waitForReaperProcessGone(t, nestedPID)
}

func TestReaperTimerCancellationReapsSleepDescendants(t *testing.T) {
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	dir := t.TempDir()
	sleepLog := filepath.Join(dir, "sleep-pids")
	sleepPath := filepath.Join(dir, "sleep")
	script := "#!/bin/sh\nprintf '%s\\n' \"$$\" >> " + reaperShellQuote(sleepLog) + "\nexec " + reaperShellQuote(realSleep) + " \"$@\"\n"
	if err := os.WriteFile(sleepPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin, _ := writeReaperStub(t)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.helperPaths = testReaperHelpers(t, reaperHelperPaths{sleep: sleepPath})
	if err := r.register("timer-ctr", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	closeReaperForTest(t, r)

	data, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("read sleep PID log: %v", err)
	}
	pids := strings.Fields(string(data))
	if len(pids) == 0 {
		t.Fatal("timer did not start a sleep descendant")
	}
	for _, field := range pids {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			t.Fatalf("sleep PID log = %q", data)
		}
		waitForReaperProcessGone(t, pid)
	}
}

func TestReaperShellBoundsPgrepLookup(t *testing.T) {
	dir := t.TempDir()
	pgrepPath := filepath.Join(dir, "pgrep")
	helperChildPath := filepath.Join(dir, "helper-child.pid")
	if err := os.WriteFile(pgrepPath, []byte("#!/bin/sh\nsleep 5 &\necho \"$!\" > "+reaperShellQuote(helperChildPath)+"\nwait\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin, logPath := writeReaperStub(t)
	const first = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const second = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	childPIDPath := filepath.Join(dir, "child.pid")
	backendScript := "#!/bin/sh\necho \"$@\" >> " + reaperShellQuote(logPath) + "\nif [ \"$1\" = \"rm\" ] && [ \"$3\" = " + reaperShellQuote(first) + " ]; then\n" +
		"  sh -c 'sleep 5' &\n" +
		"  echo \"$!\" > " + reaperShellQuote(childPIDPath) + "\n" +
		"  wait\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(backendScript), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "rm")
	r.helperPaths = testReaperHelpers(t, reaperHelperPaths{pgrep: pgrepPath})
	r.timeoutSeconds = 1
	if err := r.register(first, ""); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := r.register(second, ""); err != nil {
		t.Fatalf("register second: %v", err)
	}
	started := time.Now()
	closeReaperForTest(t, r)
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("reaper shell cleanup took %s, want bounded pgrep lookup", elapsed)
	}
	waitForPath(t, childPIDPath)
	childPID := readReaperPID(t, childPIDPath)
	waitForPath(t, helperChildPath)
	helperChildPID := readReaperPID(t, helperChildPath)
	waitForReaperProcessGone(t, childPID)
	waitForReaperProcessGone(t, helperChildPID)
	waitForLogLines(t, logPath, "rm --force "+first, "rm --force "+second)
}

func TestReaperShellRejectsInvalidPgrepPID(t *testing.T) {
	dir := t.TempDir()
	pgrepPath := filepath.Join(dir, "pgrep")
	if err := os.WriteFile(pgrepPath, []byte("#!/bin/sh\necho not-a-pid\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin, logPath := writeReaperStub(t)
	const first = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const second = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	childPIDPath := filepath.Join(dir, "child.pid")
	backendScript := "#!/bin/sh\necho \"$@\" >> " + reaperShellQuote(logPath) + "\nif [ \"$1\" = \"rm\" ] && [ \"$3\" = " + reaperShellQuote(first) + " ]; then\n" +
		"  sh -c 'sleep 30' &\n" +
		"  echo \"$!\" > " + reaperShellQuote(childPIDPath) + "\n" +
		"  wait\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(backendScript), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "rm")
	r.helperPaths = testReaperHelpers(t, reaperHelperPaths{pgrep: pgrepPath})
	r.timeoutSeconds = 1
	if err := r.register(first, ""); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := r.register(second, ""); err != nil {
		t.Fatalf("register second: %v", err)
	}
	closeReaperForTest(t, r)
	waitForPath(t, childPIDPath)
	childPID := readReaperPID(t, childPIDPath)
	waitForReaperProcessGone(t, childPID)
	waitForLogLines(t, logPath, "rm --force "+first, "rm --force "+second)
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

func TestReaperDockerGenerationRequiresImmutableID(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "docker")
	creation := "0123456789abcdef"
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then\n" +
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
	waitForLogLines(t, logPath, "inspect ctr")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "rm --force") {
			t.Fatalf("Docker generation-guarded entry fell back to name: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
