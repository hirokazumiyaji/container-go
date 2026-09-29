//go:build !windows

package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

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
	deadline := time.Now().Add(5 * time.Second)
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
	r.closeStdin()

	waitForLogLines(t, logPath, "delete --force ctr-one", "delete --force ctr-two")
}

func TestReaperDeletesRegisteredDockerIDOnEOF(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")
	id := strings.Repeat("ab", 32)

	if err := r.register(id, ""); err != nil {
		t.Fatalf("register Docker ID: %v", err)
	}
	r.closeStdin()

	waitForLogLines(t, logPath, "rm --force "+id)
}

func TestRegisterWithGlobalReaperLogsValidationFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "docker")
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, binary)
		globalReapersMu.Unlock()
	})

	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	if err := registerWithGlobalReaper(binary, "rm", "bad id", ""); err == nil {
		t.Fatal("registration unexpectedly succeeded")
	}

	for _, want := range []string{
		"container-go: reaper registration failed",
		`invalid container id "bad id"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log = %q, want %q", logs.String(), want)
		}
	}
}

func TestPreRegisterWithGlobalReaperLogsValidationFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "container")
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, binary)
		globalReapersMu.Unlock()
	})

	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	if err := preRegisterWithGlobalReaper(binary, "delete", "bad id", "0123456789abcdef"); err == nil {
		t.Fatal("pre-registration unexpectedly succeeded")
	}
	if !strings.Contains(logs.String(), "reaper pre-registration failed") {
		t.Fatalf("log = %q, want pre-registration error", logs.String())
	}
}

func TestReaperRejectsInvalidID(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer r.closeStdin()

	for _, id := range []string{
		"",
		"bad id",
		"a;b",
		"x\ny",
		"-leading",
		strings.Repeat("a", 65),
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
	} {
		if err := r.register(id, ""); err == nil {
			t.Errorf("register(%q): want error", id)
		}
	}
	if err := r.register("ctr-one", "not-hex"); err == nil {
		t.Error("register bad creation: want error")
	}
}

type blockingReaperWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingReaperWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestReaperRecoveryUsesFreshBudget(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.entries = []reaperEntry{{id: "fresh-budget"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.recoverAndReplay(ctx, nil); err != nil {
		t.Fatalf("recover with canceled caller context: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force fresh-budget")
}

func TestReaperOperationLockHonorsContext(t *testing.T) {
	var lock reaperOperationLock
	lock.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := lock.LockContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context-bounded lock took %s", elapsed)
	}
	lock.Unlock()
}

func TestReaperWriteIsContextBounded(t *testing.T) {
	writer := &blockingReaperWriter{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- writeReaperRecord(ctx, writer, "entry\n")
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, errReaperWriteTimeout) {
			t.Fatalf("write error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not honor its context budget")
	}
	close(writer.release)
}

func TestReaperInspectProjectionIgnoresPATHHelpers(t *testing.T) {
	dir := t.TempDir()
	bin, logPath := writeReaperStub(t)
	helperDir := filepath.Join(dir, "helpers")
	if err := os.Mkdir(helperDir, 0o700); err != nil {
		t.Fatal(err)
	}
	invoked := filepath.Join(dir, "helper-invoked")
	for _, name := range []string{"awk", "sed", "sleep", "pgrep", "lockf", "flock", "ps", "tr", "rm"} {
		path := filepath.Join(helperDir, name)
		script := "#!/bin/sh\necho " + name + " >> " + invoked + "\nexit 99\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Replace the stub with a generation-bearing inspect response.
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nif [ \"$1\" = inspect ]; then echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.register("projection-ctr", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force projection-ctr")
	if data, err := os.ReadFile(invoked); err == nil {
		t.Fatalf("PATH helper was used for projection: %s", data)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestReaperAppleNameLockCoversReplacement(t *testing.T) {
	dir := t.TempDir()
	name := "reaper-lock-" + newContainerName()
	creation := "0123456789abcdef"
	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	inspectPath := filepath.Join(dir, "inspect.started")
	allowDeletePath := filepath.Join(dir, "allow.delete")
	replacementPath := filepath.Join(dir, "replacement.acquired")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  : > " + inspectPath + "\n" +
		"  echo '  \"" + creationLabel + "\": \"" + creation + "\"'\n" +
		"  while [ ! -e " + allowDeletePath + " ]; do sleep 0.01; done\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForFileExists(t, inspectPath)

	acquired := make(chan error, 1)
	attempting := make(chan struct{})
	go func() {
		close(attempting)
		unlock, err := lockName(context.Background(), name)
		if err != nil {
			acquired <- err
			return
		}
		_ = os.WriteFile(replacementPath, []byte("replacement"), 0o600)
		unlock()
		acquired <- nil
	}()
	<-attempting
	select {
	case <-time.After(100 * time.Millisecond):
	case <-acquired:
		t.Fatal("replacement acquired the name lock before reaper delete")
	}
	if _, err := os.Stat(replacementPath); err == nil {
		t.Fatal("replacement acquired the name lock before reaper delete")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowDeletePath, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLogLines(t, logPath, "delete --force "+name)
	if err := <-acquired; err != nil {
		t.Fatalf("replacement lock: %v", err)
	}
	if _, err := os.Stat(replacementPath); err != nil {
		t.Fatalf("replacement did not acquire lock after delete: %v", err)
	}
}

func waitForFileExists(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestReaperRegisterGateTimeoutKeepsIntentAndReconciles(t *testing.T) {
	previous := reaperOperationLockTimeout
	reaperOperationLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reaperOperationLockTimeout = previous })

	var spawns atomic.Int32
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd {
		spawns.Add(1)
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	if err := r.register("existing", ""); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	r.mu.Lock()
	old := r.cmd
	r.mu.Unlock()
	if old == nil {
		t.Fatal("initial register did not spawn reaper")
	}

	r.opMu.Lock()
	result := make(chan error, 1)
	go func() { result <- r.register("pending", "") }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gate error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		r.opMu.Unlock()
		t.Fatal("registration did not honor the gate timeout")
	}
	r.mu.Lock()
	entries := len(r.entries)
	r.mu.Unlock()
	if entries != 2 {
		r.opMu.Unlock()
		t.Fatalf("durable entries = %d, want 2", entries)
	}
	r.opMu.Unlock()
	waitForReaperReplacement(t, r, old)
	if spawns.Load() < 2 {
		t.Fatalf("spawns = %d, want reconciliation", spawns.Load())
	}
	r.closeStdin()
	waitForReaperExit(t, r)
}

func waitForReaperExit(t *testing.T, r *reaper) {
	t.Helper()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not exit")
	}
	r.watchers.Wait()
}

func TestReaperRecoveryRetriesUntilSuccess(t *testing.T) {
	previous := initialReaperSpawnBackoff
	initialReaperSpawnBackoff = 10 * time.Millisecond
	t.Cleanup(func() { initialReaperSpawnBackoff = previous })

	var attempts atomic.Int32
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd {
		if attempts.Add(1) <= maxReaperSpawnFailures+1 {
			return nil
		}
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	if err := r.register("retry-until-success", ""); err == nil {
		t.Fatal("initial recovery unexpectedly succeeded")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		live := r.process != nil && !channelClosed(r.process.exited)
		r.mu.Unlock()
		if live {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	live := r.process != nil && !channelClosed(r.process.exited)
	r.mu.Unlock()
	if !live {
		t.Fatalf("reaper did not retry until success; attempts=%d", attempts.Load())
	}
	r.closeStdin()
	waitForReaperExit(t, r)
}

func TestReaperCompletionStopsPendingRecheck(t *testing.T) {
	bin, logPath, generationPath := writeGenerationReaperStub(t, false)
	if err := os.WriteFile(generationPath, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.registerPending("completed", "0123456789abcdef"); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	if err := r.completePending("completed", "0123456789abcdef"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect completed", "delete --force completed")
}

func TestReaperPendingEntryRechecksLateCreate(t *testing.T) {
	bin, logPath, generationPath := writeGenerationReaperStub(t, false)
	if err := os.WriteFile(generationPath, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.pendingAttempts = 3
	if err := r.registerPending("late-create", "0123456789abcdef"); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect late-create", "delete --force late-create")
}

func TestReaperPendingEntryRechecksAfterCreateFinishes(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	createdPath := filepath.Join(dir, "created")
	generationPath := filepath.Join(dir, "generation")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  if [ ! -e " + createdPath + " ]; then\n" +
		"    : > " + createdPath + "\n" +
		"    (sleep 1; printf '0123456789abcdef\\n' > " + generationPath + ") &\n" +
		"    exit 1\n" +
		"  fi\n" +
		"  printf '    \"" + creationLabel + "\": \"%s\"\\n' \"$(cat " + generationPath + ")\"\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.pendingAttempts = 4
	r.timeoutSeconds = 5
	if err := r.registerPending("late-create", "0123456789abcdef"); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect late-create", "delete --force late-create")
}

func TestReaperRespawnsAndReplaysAfterUnexpectedExit(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	for _, id := range []string{"before-crash", "second-entry"} {
		if err := r.register(id, ""); err != nil {
			t.Fatalf("register %q: %v", id, err)
		}
	}

	r.mu.Lock()
	oldCmd := r.cmd
	r.mu.Unlock()
	r.killForTest()
	waitForReaperReplacement(t, r, oldCmd)

	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force before-crash", "delete --force second-entry")
}

func waitForReaperReplacement(t *testing.T, r *reaper, old *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		replaced := r.cmd != nil && r.cmd != old
		r.mu.Unlock()
		if replaced {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaper child was not replaced after unexpected exit")
}

func TestReaperIntentionalEOFDoesNotRespawn(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	if err := r.register("eof-once", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	r.mu.Lock()
	oldCmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	r.closeStdin()
	<-exited

	// Give the exit watcher time to observe EOF before checking that it
	// treated the close as intentional rather than an unexpected exit.
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	current := r.cmd
	r.mu.Unlock()
	if current != oldCmd {
		t.Fatal("reaper respawned after intentional EOF")
	}
	waitForLogLines(t, logPath, "delete --force eof-once")
}

func TestReaperRegistrationRetryReplaysRetainedEntries(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.entries = []reaperEntry{{id: "retained", creation: ""}}
	r.spawnFailures = maxReaperSpawnFailures
	r.gaveUp = true

	if err := r.register("retained", ""); err != nil {
		t.Fatalf("retry after spawn failure: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force retained")
}

const reaperTestImmutableID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type reaperBoundaryRunner struct {
	*fakeRunner
	binary string
	onRun  func([]string) ([]byte, error)
}

func (r *reaperBoundaryRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		stdout, err := r.onRun(args)
		return stdout, nil, err
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *reaperBoundaryRunner) External() bool         { return true }
func (r *reaperBoundaryRunner) ExternalBinary() string { return r.binary }

func isolateGlobalReapers(t *testing.T) {
	t.Helper()
	t.Setenv("CONTAINERGO_KEEP", "")
	globalReapersMu.Lock()
	previous := globalReapers
	globalReapers = make(map[string]*reaper)
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		current := globalReapers
		globalReapers = previous
		globalReapersMu.Unlock()
		for _, r := range current {
			r.closeStdin()
		}
	})
}

func closeGlobalReaper(binary string) error {
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return errors.New("reaper was not registered before run")
	}
	r.closeStdin()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			return errors.New("reaper did not finish after parent EOF")
		}
	}
	return nil
}

func runCreation(args []string) string {
	for i, arg := range args {
		if arg == "--label" && i+1 < len(args) {
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				return value
			}
		}
	}
	return ""
}

func writeGenerationReaperStub(t *testing.T, docker bool) (binPath, logPath, generationPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "container")
	logPath = filepath.Join(dir, "calls.log")
	generationPath = filepath.Join(dir, "generation")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  generation=$(cat " + generationPath + ")\n" +
		"  printf '    \"" + creationLabel + "\": \"%s\"\\n' \"$generation\"\n"
	if docker {
		script += "  printf '    \"Id\": \"" + reaperTestImmutableID + "\"\\n'\n"
	}
	script += "fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, logPath, generationPath
}

func TestRunAbortsCreateWhenPreRegistrationGateTimesOut(t *testing.T) {
	previous := reaperOperationLockTimeout
	reaperOperationLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reaperOperationLockTimeout = previous })
	isolateGlobalReapers(t)

	binary := filepath.Join(t.TempDir(), "container")
	reaper := newReaper(binary, "delete")
	reaper.opMu.Lock()
	globalReapersMu.Lock()
	globalReapers[binary] = reaper
	globalReapersMu.Unlock()
	t.Cleanup(func() { reaper.opMu.Unlock() })

	var createCalls atomic.Int32
	runner := &reaperBoundaryRunner{
		fakeRunner: newTestRunner(),
		binary:     binary,
		onRun: func([]string) ([]byte, error) {
			createCalls.Add(1)
			return nil, nil
		},
	}
	runner.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine", withRunner(runner), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "reaper pre-registration") {
		t.Fatalf("Run error = %v, want pre-registration failure", err)
	}
	if got := createCalls.Load(); got != 0 {
		t.Fatalf("backend create calls = %d, want 0", got)
	}
}

func TestRunPreRegistersReaperBeforeCreate(t *testing.T) {
	tests := []struct {
		name       string
		engine     engine
		docker     bool
		deleteCall string
	}{
		{name: "apple", engine: appleEngine{}, deleteCall: "delete --force pre-register-ctr"},
		{name: "docker", engine: dockerEngine{}, docker: true, deleteCall: "rm --force " + reaperTestImmutableID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, logPath, generationPath := writeGenerationReaperStub(t, tt.docker)
			isolateGlobalReapers(t)
			base := newTestRunner()
			base.imagePresent = true
			var runGeneration string
			runner := &reaperBoundaryRunner{
				fakeRunner: base,
				binary:     bin,
				onRun: func(args []string) ([]byte, error) {
					creation := runCreation(args)
					if !creationRE.MatchString(creation) {
						return nil, errors.New("run did not carry a valid generation")
					}
					runGeneration = creation
					if err := os.WriteFile(generationPath, []byte(creation), 0o600); err != nil {
						return nil, err
					}
					// Simulate the parent receiving EOF at the exact boundary
					// between backend run success and the old registration.
					if err := closeGlobalReaper(bin); err != nil {
						return nil, err
					}
					if tt.docker {
						return []byte(reaperTestImmutableID + "\n"), nil
					}
					return []byte("pre-register-ctr\n"), nil
				},
			}

			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("pre-register-ctr"),
				WithPullPolicy(PullNever),
				withRunner(runner),
				withEngine(tt.engine),
			)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr.creation != runGeneration {
				t.Fatalf("handle generation = %q, run generation = %q", ctr.creation, runGeneration)
			}
			waitForLogLines(t, logPath, "inspect pre-register-ctr", tt.deleteCall)
		})
	}
}

func TestRunPreRegistrationPreservesNameConflict(t *testing.T) {
	tests := []struct {
		name       string
		engine     engine
		docker     bool
		conflict   string
		deleteCall string
	}{
		{
			name:       "apple",
			engine:     appleEngine{},
			conflict:   `container with id "conflict-ctr" already exists`,
			deleteCall: "delete --force conflict-ctr",
		},
		{
			name:       "docker",
			engine:     dockerEngine{},
			docker:     true,
			conflict:   `Conflict. The container name "/conflict-ctr" is already in use`,
			deleteCall: "rm --force " + reaperTestImmutableID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, logPath, generationPath := writeGenerationReaperStub(t, tt.docker)
			isolateGlobalReapers(t)
			base := newTestRunner()
			base.imagePresent = true
			runner := &reaperBoundaryRunner{
				fakeRunner: base,
				binary:     bin,
				onRun: func(args []string) ([]byte, error) {
					if !creationRE.MatchString(runCreation(args)) {
						return nil, errors.New("run did not carry a valid generation")
					}
					if err := os.WriteFile(generationPath, []byte("ffffffffffffffff"), 0o600); err != nil {
						return nil, err
					}
					if err := closeGlobalReaper(bin); err != nil {
						return nil, err
					}
					return nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: tt.conflict}
				},
			}

			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("conflict-ctr"),
				WithPullPolicy(PullNever),
				withRunner(runner),
				withEngine(tt.engine),
			)
			if err == nil {
				t.Fatal("Run succeeded, want name conflict")
			}
			waitForLogLines(t, logPath, "inspect conflict-ctr")
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(logPath)
				if strings.Contains(string(data), tt.deleteCall) {
					t.Fatalf("pre-registration deleted conflicting container: %q", data)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestReaperScriptDashWithoutJobControlKeepsProcessingEntries(t *testing.T) {
	if _, err := os.Stat("/bin/dash"); err != nil {
		t.Skipf("/bin/dash unavailable: %v", err)
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho \"$@\" >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	awkPath, err := trustedReaperTool("awk")
	if err != nil {
		t.Skipf("awk unavailable: %v", err)
	}
	sleepPath, err := trustedReaperTool("sleep")
	if err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	psPath, err := trustedReaperTool("ps")
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	trPath, err := trustedReaperTool("tr")
	if err != nil {
		t.Skipf("tr unavailable: %v", err)
	}
	rmPath, err := trustedReaperTool("rm")
	if err != nil {
		t.Skipf("rm unavailable: %v", err)
	}
	pgrepPath, err := trustedReaperTool("pgrep")
	if err != nil {
		t.Skipf("pgrep unavailable: %v", err)
	}
	statusDir := t.TempDir()
	cmd := exec.Command("/bin/dash", "-c", reaperScript,
		"containergo-reaper", binPath, "delete", creationLabel, "1", "1",
		awkPath, sleepPath, "", "", rmPath, "dash-test", "main",
		psPath, trPath, statusDir, "lockf", pgrepPath, "")
	group, err := newReaperGroupOwner()
	if err != nil {
		t.Fatal(err)
	}
	prepareReaperCommand(cmd, group)
	cmd.Stdin = strings.NewReader("first\nlater\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("reaper script: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"delete --force first", "delete --force later"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("calls = %q, want %q", data, want)
		}
	}
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, `timeout="${4:-30}"`) || !strings.Contains(reaperScript, `"$sleep_bin" 0.05`) || !strings.Contains(reaperScript, "kill -KILL -") {
		t.Error("reaper script must bound each backend call with sleep/kill (no timeout(1))")
	}
	if !strings.Contains(reaperScript, "set +m") || !strings.Contains(reaperScript, "run_with_timeout() (") {
		t.Error("reaper script must keep helpers in a killable process tree")
	}
	for _, forbidden := range []string{"mktemp", "sed -n", "ps -o"} {
		if strings.Contains(reaperScript, forbidden) {
			t.Errorf("reaper script must not depend on %q", forbidden)
		}
	}
	if !strings.Contains(reaperScript, `"$pgrep_bin" -P`) {
		t.Error("reaper script must use the trusted pgrep path for descendant cleanup")
	}
	if !strings.Contains(reaperScript, `[ "$target_pgid" = "$timeout_command_pid" ]`) {
		t.Error("reaper script must not signal a process group the target does not own")
	}
	if !strings.Contains(reaperScript, `"$setsid_bin" /bin/sh -c`) {
		t.Error("reaper script must use a private process group when setsid is available")
	}
	if !strings.Contains(reaperScript, `$1 == "P"`) || !strings.Contains(reaperScript, `$1 == "C"`) {
		t.Error("reaper script must retain pending create state until completion")
	}
	if !strings.Contains(reaperScript, "field_value") || !strings.Contains(reaperScript, "CONTAINERGO_REAPER_SCRIPT") {
		t.Error("reaper script must project inspect fields without a PATH parser and hold Apple locks")
	}
	if !strings.Contains(reaperScript, `if [ "$got" != "$entry_creation" ]; then`) {
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
	r.closeStdin()
	if r.spawnFailures != 0 {
		t.Errorf("spawnFailures = %d, want 0 after success (consecutive counting)", r.spawnFailures)
	}
}

func TestReaperRegisterWithCreationValidation(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer r.closeStdin()
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
	r.closeStdin()
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
	r.closeStdin()
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
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "rm --force ctr") {
		t.Fatalf("reaper deleted by name despite an immutable Id: %q", data)
	}
}
