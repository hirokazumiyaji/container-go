//go:build !windows

package container

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reaperState is a snapshot of the process-wide reaper for one binary.
type reaperState struct {
	entries []reaperEntry
	child   bool
}

func reaperStateFor(t *testing.T, bin string) reaperState {
	t.Helper()
	globalReapersMu.Lock()
	r := globalReapers[bin]
	globalReapersMu.Unlock()
	if r == nil {
		return reaperState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return reaperState{entries: append([]reaperEntry(nil), r.entries...), child: r.cmd != nil}
}

func closeReaperFor(t *testing.T, bin string) {
	t.Helper()
	globalReapersMu.Lock()
	r := globalReapers[bin]
	delete(globalReapers, bin)
	globalReapersMu.Unlock()
	if r != nil {
		r.closeStdin()
	}
}

func findEntry(state reaperState, id string) (reaperEntry, bool) {
	for _, entry := range state.entries {
		if entry.id == id {
			return entry, true
		}
	}
	return reaperEntry{}, false
}

// externalAppleRunner is an external (real child process) runner for the
// Apple engine that can observe the watchdog state at a chosen moment.
type externalAppleRunner struct {
	*fakeRunner
	binary string
	// onRun runs inside the create call, before the run is answered.
	onRun func()
	// runErr fails the create.
	runErr error
	// stoppedInspect answers inspect with a stopped, owned generation
	// carrying the generation the create was labelled with.
	stoppedInspect bool
	creation       string
	// deleted records the argv of every delete call.
	deleted [][]string
	mu      sync.Mutex
}

func (r *externalAppleRunner) External() bool         { return true }
func (r *externalAppleRunner) ExternalBinary() string { return r.binary }

func (r *externalAppleRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		if _, _, err := r.fakeRunner.Run(ctx, args...); err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		for i, arg := range args {
			if i+1 < len(args) && arg == "--label" {
				if value := strings.TrimPrefix(args[i+1], creationLabel+"="); value != args[i+1] {
					r.creation = value
				}
			}
		}
		onRun := r.onRun
		r.mu.Unlock()
		if onRun != nil {
			onRun()
		}
		if r.runErr != nil {
			return nil, nil, r.runErr
		}
		return []byte("myctr\n"), nil, nil
	case "inspect":
		if r.stoppedInspect {
			r.mu.Lock()
			name, creation := args[len(args)-1], r.creation
			r.mu.Unlock()
			if creation == "" {
				creation = "0123456789abcdef"
			}
			return []byte(reuseInspectJSONWithCreation(name, "stopped", "redis:7-alpine", creation)), nil, nil
		}
	case "delete":
		r.mu.Lock()
		r.deleted = append(r.deleted, append([]string(nil), args...))
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *externalAppleRunner) deleteCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.deleted...)
}

func newExternalAppleRunner(binary string) *externalAppleRunner {
	f := newTestRunner()
	f.imagePresent = true
	return &externalAppleRunner{fakeRunner: f, binary: binary}
}

func TestRunRegistersPendingAppleTargetBeforeCreate(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	name := "pending-" + newContainerName()
	runner := newExternalAppleRunner(bin)
	var duringCreate reaperState
	runner.onRun = func() { duringCreate = reaperStateFor(t, bin) }
	t.Cleanup(func() { closeReaperFor(t, bin) })

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), withRunner(runner), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entry, ok := findEntry(duringCreate, name)
	if !ok {
		t.Fatalf("entries during create = %+v, want a pending record for the name", duringCreate.entries)
	}
	if !entry.pending {
		t.Error("record written before the create is not marked pending")
	}
	if len(entry.lockPaths) != nameLockBarrierCount {
		t.Errorf("barriers during create = %v, want %d", entry.lockPaths, nameLockBarrierCount)
	}

	after := reaperStateFor(t, bin)
	confirmed, ok := findEntry(after, name)
	if !ok {
		t.Fatalf("entries after create = %+v, want the confirmed record", after.entries)
	}
	if confirmed.pending {
		t.Error("a verified create must promote its pending record to active")
	}
}

func TestCreateNotIssuedWithdrawsPendingRecord(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	name := "locked-" + newContainerName()
	// Holding the name lock means the create is never issued, so nothing
	// can exist under the new generation and the pending record must be
	// withdrawn instead of spending the reaper's retry budget.
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	runner := newExternalAppleRunner(bin)
	t.Cleanup(func() { closeReaperFor(t, bin) })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, "redis:7-alpine",
		WithName(name), withRunner(runner), withEngine(appleEngine{})); err == nil {
		t.Fatal("Run succeeded although the name lock was held")
	}
	if state := reaperStateFor(t, bin); len(state.entries) != 0 {
		t.Errorf("entries = %+v, want none after the create was never issued", state.entries)
	}
}

func TestFailedCreateRegistersVerifiedTargetBeforeDelete(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	name := "cleanup-" + newContainerName()
	runner := newExternalAppleRunner(bin)
	runner.stoppedInspect = true
	runner.runErr = &cli.CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "injected create failure"}

	// The watchdog must own the verified generation before the delete is
	// attempted, so a failed removal can still be retried after exit.
	var registeredBeforeDelete bool
	t.Cleanup(func() { closeReaperFor(t, bin) })

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), withRunner(runner), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded although the create failed")
	}
	state := reaperStateFor(t, bin)
	entry, ok := findEntry(state, name)
	if !ok {
		t.Fatalf("entries = %+v, want the verified generation registered", state.entries)
	}
	if entry.pending {
		t.Error("a verified cleanup target must be registered as active")
	}
	for _, call := range runner.deleteCalls() {
		if call[0] != "delete" {
			continue
		}
		if strings.Contains(strings.Join(call, " "), "--force") {
			t.Errorf("delete = %v, want no --force for a stopped generation", call)
		}
		registeredBeforeDelete = true
	}
	if len(runner.deleteCalls()) > 0 && !registeredBeforeDelete {
		t.Error("delete was issued without a registered target")
	}
}

// pendingReuseCreateRunner creates the shared container, so the reuse
// path writes its pending watchdog record before the create runs.
type pendingReuseCreateRunner struct {
	*externalAppleRunner
	name     string
	created  bool
	generate string
}

func (p *pendingReuseCreateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		p.mu.Lock()
		created, generation := p.created, p.generate
		p.mu.Unlock()
		if !created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "container not found: " + p.name}
		}
		return []byte(reuseInspectJSONWithCreation(args[len(args)-1], "running", "redis:7-alpine", generation)), nil, nil
	case "run":
		p.mu.Lock()
		for i, arg := range args {
			if i+1 < len(args) && arg == "--label" {
				if value := strings.TrimPrefix(args[i+1], creationLabel+"="); value != args[i+1] {
					p.generate = value
				}
			}
		}
		p.created = true
		p.mu.Unlock()
		return []byte("myctr\n"), nil, nil
	}
	return p.externalAppleRunner.Run(ctx, args...)
}

func TestReuseCreateRegistersPendingRecordWithoutPromotion(t *testing.T) {
	// A reuse generation is shared state: the pre-create record stays
	// pending so a crash during create is covered, but it is never
	// promoted, because a successful reuse container must survive this
	// process.
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	name := "reuse-" + newContainerName()
	runner := &pendingReuseCreateRunner{externalAppleRunner: newExternalAppleRunner(bin), name: name}
	t.Cleanup(func() { closeReaperFor(t, bin) })

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), WithReuse(),
		withRunner(runner), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	state := reaperStateFor(t, bin)
	entry, ok := findEntry(state, name)
	if !ok {
		t.Fatalf("entries = %+v, want a pending record for the created name", state.entries)
	}
	if !entry.pending {
		t.Error("a reuse record was promoted to active; the shared container would be deleted after exit")
	}
}

func TestPendingRecordDoesNotDeleteSharedReuseGeneration(t *testing.T) {
	// The child refuses a pending record whose container carries the reuse
	// marker, because a peer may have adopted it while the create settled.
	t.Setenv("CONTAINERGO_KEEP", "")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  echo '  \"" + managedLabel + "\": \"true\"'\n" +
		"  echo '  \"" + reuseLabel + "\": \"true\"'\n" +
		"  echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "shared-" + newContainerName()
	r := newReaper(bin, "delete")
	if err := r.registerPending(name, "0123456789abcdef"); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect "+name)
	waitForReaperIdle(t)

	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force "+name) {
		t.Fatalf("pending reuse generation was deleted: %q", data)
	}
}

func TestReaperRespawnsAfterUnexpectedChildExit(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	creation := "0123456789abcdef"
	if err := r.register("respawned", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.mu.Lock()
	first := r.cmd
	r.mu.Unlock()
	if first == nil {
		t.Fatal("register did not spawn a child")
	}

	// Kill the child behind the reaper's back: supervision, not a
	// registration, must notice and replay the records.
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var second *exec.Cmd
	for time.Now().Before(deadline) {
		r.mu.Lock()
		second = r.cmd
		r.mu.Unlock()
		if second != nil && second != first {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second == nil || second == first {
		t.Fatal("the reaper did not replace the child that exited unexpectedly")
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force respawned")
}

func TestReaperRegistrationWriteIsBounded(t *testing.T) {
	// A child that never reads its pipe must not block registration: the
	// write is bounded, and the failure respawns and replays the records.
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.writeTimeout = 150 * time.Millisecond
	var spawnsMu sync.Mutex
	spawns := 0
	r.command = func() *exec.Cmd {
		spawnsMu.Lock()
		spawns++
		first := spawns == 1
		spawnsMu.Unlock()
		if first {
			// A black hole: a child that never reads its pipe.
			return exec.Command("/bin/sh", "-c", "sleep 30")
		}
		return reaperCommand(bin, "delete", 2)
	}

	start := time.Now()
	for i := 0; i < 400; i++ {
		if err := r.register("bounded", "0123456789abcdef"); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 90*time.Second {
		t.Fatalf("registration took %v, want it bounded by the write timeout", elapsed)
	}
	spawnsMu.Lock()
	defer spawnsMu.Unlock()
	if spawns < 2 {
		t.Errorf("spawns = %d, want the wedged child replaced", spawns)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force bounded")
}

func TestReaperDiscardWithdrawsRecord(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	if err := r.registerPending("withdrawn", "0123456789abcdef"); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	if err := r.discard("withdrawn", "0123456789abcdef"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	r.mu.Lock()
	entries := len(r.entries)
	r.mu.Unlock()
	if entries != 0 {
		t.Errorf("entries = %d, want the withdrawn record removed", entries)
	}
	r.closeStdin()
	waitForReaperIdle(t)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "withdrawn") {
		t.Fatalf("withdrawn record was executed: %q", data)
	}
}

func TestReaperConfirmPromotesPendingRecord(t *testing.T) {
	// A confirmed record must delete even when the container is a shared
	// reuse generation, because the parent verified the target.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  echo '  \"" + managedLabel + "\": \"true\"'\n" +
		"  echo '  \"" + reuseLabel + "\": \"true\"'\n" +
		"  echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.registerPending("confirmed", "0123456789abcdef"); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	if err := r.confirm("confirmed", "0123456789abcdef"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force confirmed")
}

// waitForReaperIdle gives a reaper child that already saw EOF time to
// finish before a test asserts that it did nothing.
func waitForReaperIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
}
