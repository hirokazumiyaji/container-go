package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type blockingReaperWriter struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	onClose   func()
}

func newBlockingReaperWriter(onClose func()) *blockingReaperWriter {
	return &blockingReaperWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
		onClose: onClose,
	}
}

func (w *blockingReaperWriter) Write(p []byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func (w *blockingReaperWriter) Close() error {
	w.closeOnce.Do(func() {
		if w.onClose != nil {
			w.onClose()
		}
		close(w.release)
	})
	return nil
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

func TestReaperRejectsInvalidID(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	for _, id := range []string{"", "bad id", "a;b", "x\ny", "-leading"} {
		if err := r.register(id, ""); err == nil {
			t.Errorf("register(%q): want error", id)
		}
	}
	if err := r.register("ctr-one", "not-hex"); err == nil {
		t.Error("register bad creation: want error")
	}

	dockerID := strings.Repeat("ab", 32)
	if err := r.register(dockerID, ""); err != nil {
		t.Fatalf("register full Docker ID: %v", err)
	}
	if err := r.unregister(dockerID, ""); err != nil {
		t.Fatalf("unregister full Docker ID: %v", err)
	}
}

func TestReaperRespawnsAndReRegisters(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("before-crash", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Kill the reaper process group; the next register must respawn it
	// and replay the active entries.
	r.killForTest()

	if err := r.register("after-crash", ""); err != nil {
		t.Fatalf("register after crash: %v", err)
	}
	r.closeStdin()

	waitForLogLines(t, logPath, "delete --force before-crash", "delete --force after-crash")
}

func TestReaperUnregisterCancellationIsContextBounded(t *testing.T) {
	oldWriteTimeout := reaperWriteTimeout
	reaperWriteTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reaperWriteTimeout = oldWriteTimeout })

	r := newReaper("unused", "delete")
	exited := make(chan struct{})
	writer := newBlockingReaperWriter(func() { close(exited) })
	process := &reaperProcess{
		stdin:  writer,
		exited: exited,
		pid:    12345,
		pgid:   12345,
	}
	r.process = process
	r.stdin = writer
	r.exited = exited
	r.pid, r.pgid = process.pid, process.pgid
	r.entries = []reaperEntry{{id: "stuck"}}
	r.killProcess = func(*reaperProcess) {}

	result := make(chan error, 1)
	go func() { result <- r.unregister("stuck", "") }()

	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("cancellation write did not start")
	}

	// State must remain inspectable while the pipe writer is stalled.
	stateAcquired := make(chan struct{})
	go func() {
		r.mu.Lock()
		close(stateAcquired)
		r.mu.Unlock()
	}()
	select {
	case <-stateAcquired:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("unregister held the reaper state mutex during a blocked write")
	}

	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("unregister remained blocked on a stalled reaper reader")
	}
}

func TestReaperOperationCancellationWhileWaitingDoesNotEnterLifecycle(t *testing.T) {
	r := newReaper("unused", "delete")
	r.entries = []reaperEntry{{id: "active"}}
	if err := r.lockOperation(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.unlockOperation()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- r.lockOperation(ctx) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting operation error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("operation lock did not honor cancellation")
	}
	r.mu.Lock()
	entries := len(r.entries)
	r.mu.Unlock()
	if entries != 1 {
		t.Fatalf("entries after canceled wait = %d, want unchanged 1", entries)
	}
}

func waitForReaperReplacement(t *testing.T, r *reaper, old *reaperProcess, entries int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		replaced := r.process != nil && r.process != old
		active := len(r.entries)
		settled := !r.reconcilePending
		r.mu.Unlock()
		if replaced && active == entries && settled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	process := r.process
	active := len(r.entries)
	pending := r.reconcilePending
	r.mu.Unlock()
	t.Fatalf("reaper did not reconcile: replaced=%t active=%d pending=%t process=%p", process != nil && process != old, active, pending, process)
}

func TestReaperUnregisterContextNoop(t *testing.T) {
	r := newReaper("unused", "delete")
	if err := r.unregisterContext(context.Background(), reaperEntry{id: "missing"}); err != nil {
		t.Fatalf("unregister missing context entry: %v", err)
	}
}

func TestReaperRegisterGateTimeoutKeepsEntryAndReconciles(t *testing.T) {
	oldLockTimeout := reaperOperationLockTimeout
	reaperOperationLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reaperOperationLockTimeout = oldLockTimeout })

	var spawns atomic.Int32
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd {
		spawns.Add(1)
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	if err := r.registerContext(context.Background(), reaperEntry{id: "existing"}); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	r.mu.Lock()
	old := r.process
	r.mu.Unlock()
	if old == nil {
		t.Fatal("initial register did not spawn a reaper")
	}

	r.opMu.Lock()
	gateHeld := true
	defer func() {
		if gateHeld {
			r.opMu.Unlock()
		}
	}()
	result := make(chan error, 1)
	go func() { result <- r.register("pending", "") }()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("register gate error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		r.opMu.Unlock()
		gateHeld = false
		t.Fatal("register did not time out waiting for the operation gate")
	}

	r.mu.Lock()
	active := len(r.entries)
	pending := r.reconcilePending
	r.mu.Unlock()
	if active != 2 {
		r.opMu.Unlock()
		gateHeld = false
		t.Fatalf("active entries after gate timeout = %d, want durable pending registration and existing entry", active)
	}
	if !pending {
		r.opMu.Unlock()
		gateHeld = false
		t.Fatal("register gate timeout did not arrange reconciliation")
	}

	r.opMu.Unlock()
	gateHeld = false
	waitForReaperReplacement(t, r, old, 2)
	if got := spawns.Load(); got < 2 {
		t.Fatalf("reaper spawns = %d, want replacement after gate timeout", got)
	}
}

func TestReaperUnregisterGateTimeoutCompletesWithoutReplay(t *testing.T) {
	oldLockTimeout := reaperOperationLockTimeout
	reaperOperationLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reaperOperationLockTimeout = oldLockTimeout })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "active.log")
	readerPath := filepath.Join(dir, "reader.sh")
	reader := "#!/bin/sh\n" +
		"awk 'substr($0, 1, 2) == \"+ \" { active[substr($0, 3)] = 1; next } " +
		"substr($0, 1, 2) == \"- \" { delete active[substr($0, 3)]; next } " +
		"END { for (entry in active) print \"active \" entry }' >> " + reaperShellQuote(logPath) + "\n"
	if err := os.WriteFile(readerPath, []byte(reader), 0o755); err != nil {
		t.Fatal(err)
	}

	var spawns atomic.Int32
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd {
		spawns.Add(1)
		return exec.Command(readerPath)
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	for _, id := range []string{"completed", "active"} {
		if err := r.registerContext(context.Background(), reaperEntry{id: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	r.mu.Lock()
	old := r.process
	r.mu.Unlock()
	if old == nil {
		t.Fatal("initial register did not spawn a reaper")
	}

	r.opMu.Lock()
	gateHeld := true
	defer func() {
		if gateHeld {
			r.opMu.Unlock()
		}
	}()
	result := make(chan error, 1)
	go func() { result <- r.unregister("completed", "") }()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unregister gate error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		r.opMu.Unlock()
		gateHeld = false
		t.Fatal("unregister did not time out waiting for the operation gate")
	}

	r.mu.Lock()
	active := len(r.entries)
	completed := false
	for _, entry := range r.completed {
		if entry == (reaperEntry{id: "completed"}) {
			completed = true
			break
		}
	}
	pending := r.reconcilePending
	r.mu.Unlock()
	if active != 1 {
		r.opMu.Unlock()
		gateHeld = false
		t.Fatalf("active entries after gate timeout = %d, want completed entry removed", active)
	}
	if !completed {
		r.opMu.Unlock()
		gateHeld = false
		t.Fatal("unregister gate timeout did not durably record completion")
	}
	if !pending {
		r.opMu.Unlock()
		gateHeld = false
		t.Fatal("unregister gate timeout did not arrange reconciliation")
	}

	r.opMu.Unlock()
	gateHeld = false
	waitForReaperReplacement(t, r, old, 1)
	if got := spawns.Load(); got < 2 {
		t.Fatalf("reaper spawns = %d, want replacement after gate timeout", got)
	}
	r.mu.Lock()
	completedAfter := false
	for _, entry := range r.completed {
		if entry == (reaperEntry{id: "completed"}) {
			completedAfter = true
			break
		}
	}
	r.mu.Unlock()
	if !completedAfter {
		t.Fatal("reconciliation discarded the durable completed entry")
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "active completed") {
		t.Fatalf("completed entry was replayed after gate timeout: %q", data)
	}
	if !strings.Contains(string(data), "active active") {
		t.Fatalf("replacement replay log = %q, want active entry", data)
	}
}

func TestReaperCanceledRecoveryReplacesProcess(t *testing.T) {
	oldRecoveryTimeout := reaperRecoveryTimeout
	reaperRecoveryTimeout = time.Second
	t.Cleanup(func() { reaperRecoveryTimeout = oldRecoveryTimeout })

	spawns := 0
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd {
		spawns++
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	if err := r.register("active", ""); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	r.mu.Lock()
	old := r.process
	r.mu.Unlock()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.recoverAndReplay(canceled, old); err != nil {
		t.Fatalf("canceled recovery: %v", err)
	}
	if spawns != 2 {
		t.Fatalf("reaper spawns = %d, want replacement after canceled recovery", spawns)
	}
	r.mu.Lock()
	replaced := r.process != old
	r.mu.Unlock()
	if !replaced {
		t.Fatal("canceled recovery retained the detached process")
	}
}

func TestReaperCanceledReplayStillRespawns(t *testing.T) {
	oldRecoveryTimeout := reaperRecoveryTimeout
	reaperRecoveryTimeout = 2 * time.Second
	t.Cleanup(func() { reaperRecoveryTimeout = oldRecoveryTimeout })

	spawns := 0
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd {
		spawns++
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	r.entries = []reaperEntry{{id: "active"}}
	r.mu.Lock()
	if err := r.spawnLocked(); err != nil {
		r.mu.Unlock()
		t.Fatalf("initial spawn: %v", err)
	}
	old := r.process
	r.mu.Unlock()

	killed := make(chan struct{})
	var killOnce sync.Once
	r.killProcess = func(process *reaperProcess) {
		killOnce.Do(func() { close(killed) })
		process.terminate()
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	// Keep the lifecycle owner busy while cancellation arrives after the
	// old child has been detached. Recovery must still get a fresh budget.
	if err := r.lockOperation(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.unlockOperation()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- r.recoverAndReplay(ctx, old) }()
	select {
	case <-killed:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("recovery did not begin shutting down the old process")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("canceled replay recovery: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled replay recovery did not finish")
	}
	if spawns != 2 {
		t.Fatalf("reaper spawns = %d, want replacement after canceled replay", spawns)
	}
	r.mu.Lock()
	replaced := r.process != old
	r.mu.Unlock()
	if !replaced {
		t.Fatal("canceled replay did not replace the detached process")
	}
}

func TestReaperDelayedRegistrationAndUnregisterDoNotSignalReapedProcess(t *testing.T) {
	r := newReaper("unused", "delete")
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})
	r.command = func() *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "exit 0")
	}

	// Start and fully reap a child, then leave it as the current process.
	// The next lifecycle operation must observe the cleared identity, not
	// use the old numeric PID/PGID for a replacement signal.
	r.mu.Lock()
	err := r.spawnLocked()
	old := r.process
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("spawn short-lived reaper: %v", err)
	}
	<-old.exited
	r.mu.Lock()
	if r.pid != 0 || r.pgid != 0 {
		t.Fatalf("reaped identity = pid:%d pgid:%d, want 0/0", r.pid, r.pgid)
	}
	r.mu.Unlock()
	if old.pid != 0 || old.pgid != 0 {
		t.Fatalf("process identity after reap = pid:%d pgid:%d, want 0/0", old.pid, old.pgid)
	}

	signalCalls := 0
	r.killProcess = func(*reaperProcess) {
		signalCalls++
	}
	r.command = func() *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	r.entries = []reaperEntry{{id: "active"}}
	if err := r.unregister("active", ""); err != nil {
		t.Fatalf("delayed unregister: %v", err)
	}
	if err := r.register("later", ""); err != nil {
		t.Fatalf("delayed register: %v", err)
	}
	if err := r.unregister("later", ""); err != nil {
		t.Fatalf("delayed unregister: %v", err)
	}
	if signalCalls != 0 {
		t.Fatalf("reaped process signal calls = %d, want 0", signalCalls)
	}
	r.mu.Lock()
	replaced := r.process != old
	r.mu.Unlock()
	if !replaced {
		t.Fatal("delayed registration did not replace the reaped process")
	}

	r.closeStdin()
	waitForReaperExit(t, r)
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, `sleep "$killer_delay"`) ||
		!strings.Contains(reaperScript, "kill_descendants") ||
		!strings.Contains(reaperScript, `kill -KILL -"$pipeline_group"`) ||
		!strings.Contains(reaperScript, "stop_killer") {
		t.Error("reaper script must bound each backend call with an owned process group and descendant cleanup")
	}
	// The creation label must be read as a structural JSON field, anchored
	// at line start on the quoted key, and compared for exact equality.
	if !strings.Contains(reaperScript, `s/^[[:space:]]*\"$key\"[[:space:]]*:`) {
		t.Error("reaper script must anchor the creation label match on the quoted key")
	}
	if !strings.Contains(reaperScript, `[ "$got" = "$creation" ] || continue`) {
		t.Error("reaper script must compare the extracted generation exactly")
	}
}

// The handshake file the supervisor waits for is published through a staging
// file. Both names must come from mktemp: a predictable sibling path can be
// replaced with a symlink by another local user in a shared TMPDIR.
func TestReaperScriptAllocatesHandshakeFilesSecurely(t *testing.T) {
	if strings.Contains(reaperScript, `group_value="$group_file`) {
		t.Error("reaper script must not stage the handshake through a predictable sibling path")
	}
	if !strings.Contains(reaperScript, `group_value=$(mktemp `) {
		t.Error("reaper script must allocate the handshake staging file with mktemp")
	}
}

func TestReaperDisablesMonitorMode(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "shellopts.log")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"/bin/sleep 0.05\n" +
		"printf 'called:%s\\n' \"$SHELLOPTS\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.command = func() *exec.Cmd {
		cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", binPath, "delete", breQuote(creationLabel))
		cmd.Env = append(os.Environ(), "SHELLOPTS=monitor")
		return cmd
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	if err := r.register("monitor-test", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	// EOF makes the shell flush the active set to the backend.
	r.closeStdin()
	waitForLogLines(t, logPath, "called:")
	data, _ := os.ReadFile(logPath)
	opts := strings.TrimPrefix(strings.TrimSpace(string(data)), "called:")
	for _, option := range strings.Split(opts, ":") {
		if option == "monitor" {
			t.Fatalf("reaper helper inherited monitor mode from SHELLOPTS: %q", opts)
		}
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
		t.Fatal("reaper child did not exit")
	}
}

func TestReaperUnregisterRemovesCompletedEntryFromEOF(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("completed", ""); err != nil {
		t.Fatalf("register completed: %v", err)
	}
	if err := r.unregister("completed", ""); err != nil {
		t.Fatalf("unregister completed: %v", err)
	}
	if err := r.register("active", ""); err != nil {
		t.Fatalf("register active: %v", err)
	}
	if got := len(r.entries); got != 1 {
		t.Fatalf("active entries = %d, want 1", got)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "delete --force completed") {
		t.Fatalf("reaper deleted a completed entry: %q", data)
	}
	if !strings.Contains(string(data), "delete --force active") {
		t.Fatalf("reaper did not delete active entry: %q", data)
	}
}

func TestReaperRespawnReplaysOnlyActiveEntries(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("completed", ""); err != nil {
		t.Fatalf("register completed: %v", err)
	}
	if err := r.unregister("completed", ""); err != nil {
		t.Fatalf("unregister completed: %v", err)
	}
	if err := r.register("active", ""); err != nil {
		t.Fatalf("register active: %v", err)
	}

	r.killForTest()
	if err := r.register("after-crash", ""); err != nil {
		t.Fatalf("register after crash: %v", err)
	}
	r.closeStdin()
	waitForReaperExit(t, r)

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "delete --force completed") {
		t.Fatalf("respawn replayed a completed entry: %q", data)
	}
	for _, want := range []string{"delete --force active", "delete --force after-crash"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("respawn log = %q, want %q", data, want)
		}
	}
	if got := strings.Count(string(data), "delete --force active"); got != 1 {
		t.Errorf("active deletes after respawn = %d, want 1; log = %q", got, data)
	}
}

func TestReaperConcurrentUnregisterDuringReplay(t *testing.T) {
	_, logPath := writeReaperStub(t)
	readerPath := filepath.Join(t.TempDir(), "reader.sh")
	reader := "#!/bin/sh\n" +
		"awk 'substr($0, 1, 2) == \"+ \" { active[substr($0, 3)] = 1; next } " +
		"substr($0, 1, 2) == \"- \" { delete active[substr($0, 3)]; next } " +
		"END { for (entry in active) print \"active \" entry }' >> " + logPath + "\n"
	if err := os.WriteFile(readerPath, []byte(reader), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd { return exec.Command(readerPath) }
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	const entries = 128
	for i := 0; i < entries; i++ {
		if err := r.register(fmt.Sprintf("replay-%d", i), ""); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	r.killForTest()

	errs := make(chan error, entries*2)
	var wg sync.WaitGroup
	for i := 0; i < entries; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				if err := r.unregister(fmt.Sprintf("replay-%d", i), ""); err != nil {
					errs <- fmt.Errorf("unregister %d: %w", i, err)
				}
				return
			}
			if err := r.register(fmt.Sprintf("replacement-%d", i), ""); err != nil {
				errs <- fmt.Errorf("replacement %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	data, _ := os.ReadFile(logPath)
	for i := 0; i < entries; i++ {
		id := fmt.Sprintf("replay-%d", i)
		line := "active " + id + "\n"
		if i%2 == 0 && strings.Contains(string(data), line) {
			t.Errorf("concurrently unregistered entry was replayed: %s", id)
		}
		if i%2 == 1 {
			if !strings.Contains(string(data), line) {
				t.Errorf("active entry missing from replay: %s", id)
			}
			replacement := fmt.Sprintf("replacement-%d", i)
			if !strings.Contains(string(data), "active "+replacement+"\n") {
				t.Errorf("new entry missing from replay: %s", replacement)
			}
		}
	}
}

func TestReaperCompletedEntriesAreBounded(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	spawns := 0
	r.command = func() *exec.Cmd {
		spawns++
		return exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", bin, "delete", breQuote(creationLabel))
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	const lifecycles = 10_000
	errs := make(chan error, lifecycles*2)
	var wg sync.WaitGroup
	for i := 0; i < lifecycles; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("ctr-%d", i)
			if err := r.register(id, ""); err != nil {
				errs <- fmt.Errorf("register %d: %w", i, err)
				return
			}
			if err := r.unregister(id, ""); err != nil {
				errs <- fmt.Errorf("unregister %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r.mu.Lock()
	activeEntries := len(r.entries)
	completedEntries := len(r.completed)
	r.mu.Unlock()
	if activeEntries != 0 {
		t.Fatalf("active entries after %d lifecycles = %d, want 0", lifecycles, activeEntries)
	}
	if spawns != 1 {
		t.Fatalf("reaper spawns for %d concurrent lifecycles = %d, want 1", lifecycles, spawns)
	}
	if completedEntries > maxReaperCompletedEntries {
		t.Fatalf("completed entries = %d, want at most %d", completedEntries, maxReaperCompletedEntries)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("reaper issued deletes for completed entries: %q", data)
	}
}

func TestReaperFailedSpawnSchedulesDurableRecovery(t *testing.T) {
	oldRecoveryTimeout := reaperRecoveryTimeout
	reaperRecoveryTimeout = 500 * time.Millisecond
	t.Cleanup(func() { reaperRecoveryTimeout = oldRecoveryTimeout })

	missing := filepath.Join(t.TempDir(), "missing-reaper-command")
	var attempts atomic.Int32
	r := newReaper("unused", "delete")
	r.backoff = func(int) time.Duration { return 10 * time.Millisecond }
	r.command = func() *exec.Cmd {
		if attempts.Add(1) <= maxReaperSpawnFailures {
			return exec.Command(missing)
		}
		return exec.Command("/bin/sh", "-c", "cat >/dev/null")
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	if err := r.register("active", ""); err == nil {
		t.Fatal("register unexpectedly succeeded")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		process := r.process
		entries := len(r.entries)
		r.mu.Unlock()
		if process != nil && processLive(process) && entries == 1 {
			if got := attempts.Load(); got <= maxReaperSpawnFailures {
				t.Fatalf("reaper attempts = %d, want scheduled retry after cooldown", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("failed spawn was not actively reconciled; attempts=%d", attempts.Load())
}

func TestReaperReconciliationBackoffEscalatesAndBounds(t *testing.T) {
	oldLockTimeout := reaperReconcileLockTimeout
	reaperReconcileLockTimeout = 5 * time.Millisecond
	t.Cleanup(func() { reaperReconcileLockTimeout = oldLockTimeout })

	r := newReaper("unused", "delete")
	r.entries = []reaperEntry{{id: "active"}}
	r.backoff = func(level int) time.Duration { return time.Duration(level) * time.Millisecond }
	r.command = func() *exec.Cmd { return exec.Command("/bin/sh", "-c", "cat >/dev/null") }
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})
	r.opMu.Lock()
	gateHeld := true
	defer func() {
		if gateHeld {
			r.opMu.Unlock()
		}
	}()
	r.requestReconcile()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		level := r.reconcileRetryLevel
		r.mu.Unlock()
		if level >= maxReaperReconcileRetryLevel {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	level := r.reconcileRetryLevel
	r.mu.Unlock()
	if level != maxReaperReconcileRetryLevel {
		t.Fatalf("reconciliation retry level = %d, want bounded level %d", level, maxReaperReconcileRetryLevel)
	}
	r.mu.Lock()
	delay := r.reconcileRetryDelayLocked()
	r.mu.Unlock()
	if delay <= 0 || delay > maxReaperSpawnBackoff {
		t.Fatalf("bounded reconciliation delay = %s, want (0,%s]", delay, maxReaperSpawnBackoff)
	}

	r.opMu.Unlock()
	gateHeld = false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		process := r.process
		pending := r.reconcilePending
		level := r.reconcileRetryLevel
		r.mu.Unlock()
		if process != nil && processLive(process) && !pending && level == 0 {
			r.closeStdin()
			waitForReaperExit(t, r)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reconciliation did not recover after bounded gate retries")
}

func waitForReaperSpawnCount(t *testing.T, spawns *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if spawns.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("reaper spawns = %d, want at least %d", spawns.Load(), want)
}

func waitForReaperReconcileSettled(t *testing.T, r *reaper, entries int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		pending, running, active := r.reconcilePending, r.reconcileRunning, len(r.entries)
		r.mu.Unlock()
		if !pending && !running && active == entries {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	pending, running, active := r.reconcilePending, r.reconcileRunning, len(r.entries)
	r.mu.Unlock()
	t.Fatalf("reconciliation did not settle: pending=%v running=%v active=%d, want false/false/%d", pending, running, active, entries)
}

// A durable intent recorded while the worker replays a snapshot is not part of
// the records that snapshot writes. The successful cycle must therefore leave
// the request pending, or the live reaper never learns about the new entry and
// still deletes the completed one on parent EOF.
func TestReaperReconcileAppliesIntentRecordedDuringReplay(t *testing.T) {
	oldWriteTimeout := reaperWriteTimeout
	reaperWriteTimeout = 2 * time.Second
	t.Cleanup(func() { reaperWriteTimeout = oldWriteTimeout })

	const entries = 8000
	dir := t.TempDir()
	var spawns atomic.Int32
	r := newReaper("unused", "delete")
	r.backoff = func(int) time.Duration { return 10 * time.Millisecond }
	// The child leaves stdin alone long enough for the snapshot to overrun the
	// pipe buffer, so the first cycle is still replaying when the intents below
	// are recorded. Each spawn appends to its own log.
	r.command = func() *exec.Cmd {
		logPath := filepath.Join(dir, fmt.Sprintf("replay-%d.log", spawns.Add(1)))
		return exec.Command("/bin/sh", "-c", "/bin/sleep 0.15; cat >> "+reaperShellQuote(logPath))
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	for i := 0; i < entries; i++ {
		r.recordRegisterIntent(reaperEntry{id: fmt.Sprintf("entry-%04d", i)})
	}
	r.requestReconcile()

	// The snapshot is cloned before the child is spawned, so an observed spawn
	// means the running cycle already replayed the older state.
	waitForReaperSpawnCount(t, &spawns, 1)
	time.Sleep(50 * time.Millisecond)
	r.recordUnregisterIntent(reaperEntry{id: "entry-0000"})
	r.requestReconcile()
	r.recordRegisterIntent(reaperEntry{id: "late"})
	r.requestReconcile()

	waitForReaperReconcileSettled(t, r, entries)
	firstLog := filepath.Join(dir, "replay-1.log")
	waitForLogLines(t, firstLog, "+ entry-0000")
	secondLog := filepath.Join(dir, "replay-2.log")
	waitForLogLines(t, secondLog, "+ late")
	second, _ := os.ReadFile(secondLog)
	if strings.Contains(string(second), "+ entry-0000") {
		t.Errorf("replay after a mid-cycle cancellation = %q, want no completed entry", second)
	}
}

func TestReaperSpawnFailuresRetryAfterBackoff(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	var nowMu sync.RWMutex
	now := time.Unix(100, 0)
	r.now = func() time.Time {
		nowMu.RLock()
		defer nowMu.RUnlock()
		return now
	}
	missing := filepath.Join(t.TempDir(), "missing-reaper-command")
	var attempts atomic.Int32
	r.command = func() *exec.Cmd {
		if attempts.Add(1) <= maxReaperSpawnFailures {
			return exec.Command(missing)
		}
		return nil
	}

	if err := r.register("first", ""); err == nil {
		t.Fatal("first registration unexpectedly succeeded")
	}
	r.mu.Lock()
	gaveUp, failures := r.gaveUp, r.spawnFailures
	entryCount := len(r.entries)
	retryAt := r.retryAt
	r.mu.Unlock()
	if !gaveUp || failures != maxReaperSpawnFailures {
		t.Fatalf("failure state = gaveUp:%v failures:%d, want true/%d", gaveUp, failures, maxReaperSpawnFailures)
	}
	if entryCount != 1 {
		t.Fatalf("active entries after failure = %d, want 1", entryCount)
	}
	nowMu.RLock()
	currentNow := now
	nowMu.RUnlock()
	if !retryAt.After(currentNow) {
		t.Fatalf("retry deadline = %s, want after %s", retryAt, currentNow)
	}
	if got := retryAt.Sub(currentNow); got != initialReaperSpawnBackoff {
		t.Fatalf("first retry backoff = %s, want %s", got, initialReaperSpawnBackoff)
	}

	if err := r.register("during-cooldown", ""); !errors.Is(err, errReaperSpawnCooldown) {
		t.Fatalf("registration during cooldown = %v, want cooldown error", err)
	}
	if got := attempts.Load(); got != maxReaperSpawnFailures {
		t.Fatalf("spawn attempts during cooldown = %d, want %d", got, maxReaperSpawnFailures)
	}

	nowMu.Lock()
	now = retryAt
	nowMu.Unlock()
	r.mu.Lock()
	r.command = nil
	r.mu.Unlock()
	if err := r.register("after-recovery", ""); err != nil {
		t.Fatalf("registration after recovery: %v", err)
	}
	r.mu.Lock()
	gaveUp, failures = r.gaveUp, r.spawnFailures
	r.mu.Unlock()
	if gaveUp || failures != 0 {
		t.Fatalf("failure state after recovery = gaveUp:%v failures:%d, want false/0", gaveUp, failures)
	}
	if got := attempts.Load(); got != maxReaperSpawnFailures {
		t.Fatalf("failed command attempts = %d, want %d", got, maxReaperSpawnFailures)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	data, _ := os.ReadFile(logPath)
	for _, want := range []string{"delete --force first", "delete --force during-cooldown", "delete --force after-recovery"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("replay log = %q, want %q", data, want)
		}
	}
}

type cancelingDeleteRunner struct {
	cancel context.CancelFunc
}

func (r *cancelingDeleteRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "delete" {
		r.cancel()
	}
	return nil, nil, nil
}

func TestTerminateUsesCallerContextForReaperGate(t *testing.T) {
	r := newReaper("unused", "delete")
	r.command = func() *exec.Cmd { return exec.Command("/bin/sh", "-c", "cat >/dev/null") }
	if err := r.register("ctr", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})

	r.opMu.Lock()
	gateHeld := true
	defer func() {
		if gateHeld {
			r.opMu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelingDeleteRunner{cancel: cancel}
	ctr := &Container{
		id:     "ctr",
		uid:    "opaque-container-id",
		runner: runner,
		eng:    appleEngine{},
		reaper: &reaperRegistration{reaper: r, entry: reaperEntry{id: "ctr"}},
	}
	result := make(chan error, 1)
	go func() { result <- ctr.Terminate(ctx) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Terminate after successful delete: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		r.opMu.Unlock()
		gateHeld = false
		t.Fatal("successful Terminate waited on a fresh background reaper gate")
	}

	r.opMu.Unlock()
	gateHeld = false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		entries := len(r.entries)
		process := r.process
		pending := r.reconcilePending
		r.mu.Unlock()
		if entries == 0 && process == nil && !pending {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("canceled Terminate did not leave durable background reconciliation")
}

func TestTerminateUnregistersReaperEntry(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	creation := "0123456789abcdef"
	if err := r.register("ctr", creation); err != nil {
		t.Fatalf("register: %v", err)
	}

	f := newTestRunner()
	f.creations = map[string]string{"ctr": creation}
	ctr := &Container{
		id:       "ctr",
		runner:   f,
		eng:      appleEngine{},
		creation: creation,
		reaper:   &reaperRegistration{reaper: r, entry: reaperEntry{id: "ctr", creation: creation}},
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := len(r.entries); got != 0 {
		t.Fatalf("active entries after Terminate = %d, want 0", got)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("reaper deleted a normally terminated entry: %q", data)
	}
}

func TestTerminateReplacementUnregistersReaperEntry(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	oldCreation := "0123456789abcdef"
	if err := r.register("ctr", oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}

	f := newTestRunner()
	f.creations = map[string]string{"ctr": "bbbbbbbbbbbbbbbb"}
	ctr := &Container{
		id:       "ctr",
		runner:   f,
		eng:      appleEngine{},
		creation: oldCreation,
		reaper:   &reaperRegistration{reaper: r, entry: reaperEntry{id: "ctr", creation: oldCreation}},
	}
	if err := ctr.Terminate(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Terminate replacement error = %v, want ErrGenerationReplaced", err)
	}
	if got := len(r.entries); got != 0 {
		t.Fatalf("active entries after replacement = %d, want 0", got)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("reaper deleted a replaced entry: %q", data)
	}
}
