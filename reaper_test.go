package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, "sleep 30") || !strings.Contains(reaperScript, "kill -9") {
		t.Error("reaper script must bound each backend call with sleep/kill (no timeout(1))")
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

func TestReaperCompletedEntriesAreBounded(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	spawns := 0
	r.command = func() *exec.Cmd {
		spawns++
		return exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", bin, "delete", breQuote(creationLabel))
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
	if got := len(r.entries); got != 0 {
		t.Fatalf("active entries after %d lifecycles = %d, want 0", lifecycles, got)
	}
	if spawns != 1 {
		t.Fatalf("reaper spawns for %d sequential lifecycles = %d, want 1", lifecycles, spawns)
	}
	if got := len(r.completed); got > maxReaperCompletedEntries {
		t.Fatalf("completed entries = %d, want at most %d", got, maxReaperCompletedEntries)
	}

	r.closeStdin()
	waitForReaperExit(t, r)
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("reaper issued deletes for completed entries: %q", data)
	}
}

func TestReaperSpawnFailuresRetryAfterBackoff(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }
	missing := filepath.Join(t.TempDir(), "missing-reaper-command")
	attempts := 0
	r.command = func() *exec.Cmd {
		attempts++
		if attempts <= maxReaperSpawnFailures {
			return exec.Command(missing)
		}
		return nil
	}

	if err := r.register("first", ""); err == nil {
		t.Fatal("first registration unexpectedly succeeded")
	}
	if !r.gaveUp || r.spawnFailures != maxReaperSpawnFailures {
		t.Fatalf("failure state = gaveUp:%v failures:%d, want true/%d", r.gaveUp, r.spawnFailures, maxReaperSpawnFailures)
	}
	if got := len(r.entries); got != 1 {
		t.Fatalf("active entries after failure = %d, want 1", got)
	}
	if !r.retryAt.After(now) {
		t.Fatalf("retry deadline = %s, want after %s", r.retryAt, now)
	}
	if got := r.retryAt.Sub(now); got != initialReaperSpawnBackoff {
		t.Fatalf("first retry backoff = %s, want %s", got, initialReaperSpawnBackoff)
	}

	if err := r.register("during-cooldown", ""); !errors.Is(err, errReaperSpawnCooldown) {
		t.Fatalf("registration during cooldown = %v, want cooldown error", err)
	}
	if attempts != maxReaperSpawnFailures {
		t.Fatalf("spawn attempts during cooldown = %d, want %d", attempts, maxReaperSpawnFailures)
	}

	now = r.retryAt
	r.command = nil
	if err := r.register("after-recovery", ""); err != nil {
		t.Fatalf("registration after recovery: %v", err)
	}
	if r.gaveUp || r.spawnFailures != 0 {
		t.Fatalf("failure state after recovery = gaveUp:%v failures:%d, want false/0", r.gaveUp, r.spawnFailures)
	}
	if attempts != maxReaperSpawnFailures {
		t.Fatalf("failed command attempts = %d, want %d", attempts, maxReaperSpawnFailures)
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
