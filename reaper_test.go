//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	requirePOSIXShell(t)
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

func TestReaperAcceptsDockerIDAndVolumeFlag(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	uid := strings.Repeat("ab", 32)
	r := newReaper(bin, "rm", "--volumes")

	if err := r.register(uid, ""); err != nil {
		t.Fatalf("register Docker ID: %v", err)
	}
	r.closeStdin()

	waitForLogLines(t, logPath, "rm --force --volumes "+uid)
}

func TestReaperRejectsInvalidID(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer r.closeStdin()

	for _, id := range []string{"", "bad id", "a;b", "x\ny", "-leading"} {
		if err := r.register(id, ""); err == nil {
			t.Errorf("register(%q): want error", id)
		}
	}
	if err := r.register("ctr-one", "not-hex"); err == nil {
		t.Error("register bad creation: want error")
	}
	if err := r.register(strings.Repeat("g", 64), ""); err == nil {
		t.Error("register non-hex Docker ID: want error")
	}
}

func TestReaperRespawnsAndReRegisters(t *testing.T) {
	requirePOSIXShell(t)
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
	r.closeStdin()

	waitForLogLines(t, logPath, "delete --force before-crash", "delete --force after-crash")
}

func TestReaperSIGKILLDoesNotStageInspectSecrets(t *testing.T) {
	stagingDir := t.TempDir()
	workDir := t.TempDir()
	started := filepath.Join(workDir, "inspect-started")
	release := filepath.Join(workDir, "release-inspect")
	done := filepath.Join(workDir, "inspect-done")
	binPath := filepath.Join(workDir, "container")
	const secret = "reaper-secret-4f8c2a"

	// Keep the fake binary and its marker outside the directory used as
	// TMPDIR so only reaper-created files are inspected below.
	t.Setenv("TMPDIR", stagingDir)
	t.Setenv("REAPER_TEST_STARTED", started)
	t.Setenv("REAPER_TEST_RELEASE", release)
	t.Setenv("REAPER_TEST_DONE", done)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  echo '  \"environment\": [\"PASSWORD=" + secret + "\"]'\n" +
		"  echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'\n" +
		"  : > \"$REAPER_TEST_STARTED\"\n" +
		"  while [ ! -e \"$REAPER_TEST_RELEASE\" ]; do sleep 1; done\n" +
		"  : > \"$REAPER_TEST_DONE\"\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	reaperKilled := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		if !reaperKilled {
			r.killForTest()
		}
	})
	if err := r.register("guarded", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForPath(t, started)
	r.killForTest()
	reaperKilled = true
	_ = os.WriteFile(release, nil, 0o600)
	// Stopping the reaper owns the complete process tree. The backend
	// inspect descendant must not survive the stopped shell.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(done); err == nil {
		t.Fatal("reaper descendant survived process-group shutdown")
	}

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(stagingDir, entry.Name()))
		if readErr != nil {
			t.Fatalf("read reaper staging file %s: %v", entry.Name(), readErr)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret remained in reaper staging file %s", entry.Name())
		}
	}
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, `sleep "$seconds"`) || !strings.Contains(reaperScript, "kill -9") {
		t.Error("reaper script must bound each backend call with sleep/kill (no timeout(1))")
	}
	// The creation label must be read as a structural JSON field, anchored
	// at line start on the quoted key, and compared for exact equality.
	if !strings.Contains(reaperScript, `s/^[[:space:]]*\"$key\"[[:space:]]*:`) {
		t.Error("reaper script must anchor the creation label match on the quoted key")
	}
	if !strings.Contains(reaperScript, `[ "$got" = "$entry_creation" ] || return 0`) {
		t.Error("reaper script must compare the extracted generation exactly")
	}
	if strings.Contains(reaperScript, "mktemp") {
		t.Error("reaper must not stage raw inspect output")
	}
}

func TestBreQuoteEscapesLabelKey(t *testing.T) {
	if got := breQuote("com.github.x-y"); got != `com\.github\.x-y` {
		t.Errorf("breQuote = %q", got)
	}
}

func TestReaperSpawnFailuresResetOnSuccess(t *testing.T) {
	requirePOSIXShell(t)
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
	requirePOSIXShell(t)
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
	requirePOSIXShell(t)
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
	requirePOSIXShell(t)
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
	requirePOSIXShell(t)
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
	r := newReaper(binPath, "rm", "--volumes")
	if err := r.register("ctr", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force --volumes "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "rm --force --volumes ctr") {
		t.Fatalf("reaper deleted by name despite an immutable Id: %q", data)
	}
}

func TestReaperPendingRegistrationRetriesSettlingCreate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	countPath := filepath.Join(dir, "inspect-count")
	binPath := filepath.Join(dir, "container")
	creation := "0123456789abcdef"
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  count=0\n" +
		"  [ -f " + countPath + " ] && count=$(cat " + countPath + ")\n" +
		"  count=$((count + 1))\n" +
		"  echo \"$count\" > " + countPath + "\n" +
		"  [ \"$count\" -ge 2 ] || exit 1\n" +
		"  echo '  \"" + creationLabel + "\": \"" + creation + "\"'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	r.pendingAttempts = 4
	if err := r.registerPending("settling", creation); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force settling")
}

func TestReaperRestartsAndReplaysAfterUnexpectedChildExit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "container")
	creation := "0123456789abcdef"
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then echo '  \"" + creationLabel + "\": \"" + creation + "\"'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("replayed", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.mu.Lock()
	old := r.cmd
	r.mu.Unlock()
	if old == nil || old.Process == nil {
		t.Fatal("reaper did not start a child")
	}
	if err := old.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		current := r.cmd
		r.mu.Unlock()
		if current != nil && current != old {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect replayed", "delete --force replayed")
}

func TestReaperCompletionRetainsActiveEntry(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	creation := "0123456789abcdef"
	if err := r.registerPending("completed", creation); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	if err := r.completePending("completed", creation); err != nil {
		t.Fatalf("completePending: %v", err)
	}
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || entries[0].pending || entries[0].shared {
		t.Fatalf("entries = %+v, want one active entry", entries)
	}
	r.closeStdin()
}

func TestReaperSharedEntryIsNotDeletedOnEOF(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "container")
	creation := "0123456789abcdef"
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then echo '  \"" + creationLabel + "\": \"" + creation + "\"'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.registerPending("shared", creation); err != nil {
		t.Fatal(err)
	}
	if err := r.markShared("shared", creation); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	time.Sleep(250 * time.Millisecond)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force shared") {
		t.Fatalf("shared entry was deleted: %q", data)
	}
}

func TestReaperUnregisterRemovesCompletedEntryFromEOF(t *testing.T) {
	requirePOSIXShell(t)
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("completed", ""); err != nil {
		t.Fatalf("register completed: %v", err)
	}
	if err := r.register("active", ""); err != nil {
		t.Fatalf("register active: %v", err)
	}
	if err := r.unregister("completed", ""); err != nil {
		t.Fatalf("unregister completed: %v", err)
	}

	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force active")

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "delete --force completed") {
		t.Fatalf("reaper deleted completed entry: %q", data)
	}
}

func TestReaperRespawnReplaysOnlyActiveEntries(t *testing.T) {
	requirePOSIXShell(t)
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
	waitForLogLines(t, logPath, "delete --force active", "delete --force after-crash")

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "delete --force completed") {
		t.Fatalf("reaper replayed completed entry after crash: %q", data)
	}
}

func TestReaperCompletedEntriesAreBounded(t *testing.T) {
	requirePOSIXShell(t)
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")

	// Anchor keeps child process alive during the batch of 10,000 register/unregister cycles.
	if err := r.register("anchor", ""); err != nil {
		t.Fatalf("register anchor: %v", err)
	}

	const lifecycles = 10_000
	for i := 0; i < lifecycles; i++ {
		id := fmt.Sprintf("ctr-%d", i)
		if err := r.register(id, ""); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		if err := r.unregister(id, ""); err != nil {
			t.Fatalf("unregister %d: %v", i, err)
		}
	}

	if err := r.unregister("anchor", ""); err != nil {
		t.Fatalf("unregister anchor: %v", err)
	}

	r.mu.Lock()
	activeCount := len(r.entries)
	r.mu.Unlock()
	if activeCount != 0 {
		t.Fatalf("active entries after %d lifecycles = %d, want 0", lifecycles, activeCount)
	}
	if !r.entriesEmpty() {
		t.Fatal("entriesEmpty() returned false, want true")
	}
}

func BenchmarkReaperLifecycle(b *testing.B) {
	dir := b.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		b.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("anchor", ""); err != nil {
		b.Fatal(err)
	}
	defer func() {
		_ = r.unregister("anchor", "")
		r.closeStdin()
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bench-%d", i)
		if err := r.register(id, ""); err != nil {
			b.Fatal(err)
		}
		if err := r.unregister(id, ""); err != nil {
			b.Fatal(err)
		}
	}
}

type fakeAppleTerminateRunner struct {
	bin      string
	notFound bool
	replaced bool
}

func (r *fakeAppleTerminateRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, nil
	}
	switch args[0] {
	case "inspect":
		if r.notFound {
			return nil, []byte("container not found"), fmt.Errorf("%w: exit status 1", ErrContainerNotFound)
		}
		target := "ctr"
		if len(args) > 1 {
			target = args[1]
		}
		gen := "0123456789abcdef"
		if r.replaced {
			gen = "fedcba9876543210"
		}
		return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"alpine"},"labels":{%q:"true",%q:%q,%q:%q}},"status":{"state":"running","networks":[]}}]`, target, target, managedLabel, sessionLabel, sessionID(), creationLabel, gen)), nil, nil
	case "delete":
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *fakeAppleTerminateRunner) External() bool         { return true }
func (r *fakeAppleTerminateRunner) ExternalBinary() string { return r.bin }

func TestTerminateAppleNotFoundUnregistersReaper(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	runner := &fakeAppleTerminateRunner{bin: bin, notFound: true}
	watchdog := newReaper(bin, "delete")
	creation := "0123456789abcdef"
	if err := watchdog.register("apple-not-found", creation); err != nil {
		t.Fatal(err)
	}
	globalReapersMu.Lock()
	globalReapers[bin] = watchdog
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		watchdog.closeStdin()
	})

	ctr := &Container{id: "apple-not-found", creation: creation, runner: runner, eng: appleEngine{}}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	watchdog.mu.Lock()
	entries := len(watchdog.entries)
	watchdog.mu.Unlock()
	if entries != 0 {
		t.Fatalf("reaper retained %d records after not found Terminate, want 0", entries)
	}
}

func TestTerminateAppleReplacedUnregistersReaper(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	runner := &fakeAppleTerminateRunner{bin: bin, replaced: true}
	watchdog := newReaper(bin, "delete")
	creation := "0123456789abcdef"
	if err := watchdog.register("apple-replaced", creation); err != nil {
		t.Fatal(err)
	}
	globalReapersMu.Lock()
	globalReapers[bin] = watchdog
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		watchdog.closeStdin()
	})

	ctr := &Container{id: "apple-replaced", creation: creation, runner: runner, eng: appleEngine{}}
	err := ctr.Terminate(context.Background())
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Terminate error = %v, want ErrGenerationReplaced", err)
	}
	watchdog.mu.Lock()
	entries := len(watchdog.entries)
	watchdog.mu.Unlock()
	if entries != 0 {
		t.Fatalf("reaper retained %d records after replaced Terminate, want 0", entries)
	}
}
