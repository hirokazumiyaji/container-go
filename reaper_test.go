//go:build !windows

package container

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
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
	r := newReaper(bin, "rm")

	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	if err := r.register(first, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register(second, ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Closing stdin is what the reaper sees when the parent process
	// dies, however it dies.
	r.closeStdin()

	waitForLogLines(t, logPath, "rm --force "+first, "rm --force "+second)
}

func TestReaperPendingEntryRechecksLateCreate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	counterPath := filepath.Join(dir, "counter")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  n=$(cat " + counterPath + " 2>/dev/null || echo 0)\n" +
		"  n=$((n + 1)); echo \"$n\" > " + counterPath + "\n" +
		"  if [ \"$n\" -lt 2 ]; then exit 1; fi\n" +
		"  echo '    \"" + creationLabel + "\": \"0123456789abcdef\",'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	r.pendingAttempts = 4
	if err := r.registerPending("late", "0123456789abcdef"); err != nil {
		t.Fatalf("registerPending: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect late", "delete --force late")
}

func TestReaperCompletionStopsPendingRecheck(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	// The completion marker changes the record to active; the inspect
	// response still proves the generation before deletion.
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("if [ \"$1\" = inspect ]; then echo '    \""+creationLabel+"\": \"0123456789abcdef\",'; exit 0; fi\n")...)
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.registerPending("complete", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := r.completePending("complete", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force complete")
}

func TestReaperPromotesPendingDockerEntryToImmutableID(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")
	if err := r.registerPending("pending-docker", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	uid := strings.Repeat("ab", 32)
	if err := r.promotePendingToDockerID("pending-docker", "0123456789abcdef", uid); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || entries[0].id != uid {
		t.Fatalf("entries = %+v, want immutable ID %q", entries, uid)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
}

type reaperDeadlineWriter struct {
	deadline time.Time
}

func (w *reaperDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (w *reaperDeadlineWriter) Write(p []byte) (int, error) {
	if wait := time.Until(w.deadline); wait > 0 {
		time.Sleep(wait)
		return 0, os.ErrDeadlineExceeded
	}
	return len(p), nil
}

func TestReaperRegistrationWritesAreBounded(t *testing.T) {
	w := &reaperDeadlineWriter{}
	start := time.Now()
	if err := writeReaperLine(w, "A\\n", 20*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write error = %v, want deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded write took %s", elapsed)
	}
}

func TestReaperEntryMemoryIsBounded(t *testing.T) {
	r := newReaper("unused", "rm")
	r.command = func() *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "while IFS= read -r line; do case \"$line\" in R#containergo-retire:*) echo \"containergo-reaper-ack:${line#R#containergo-retire:}\";; esac; done")
	}
	for i := 0; i < maxReaperEntries; i++ {
		if err := r.register(fmt.Sprintf("%064x", i+1), ""); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	overflowID := fmt.Sprintf("%064x", maxReaperEntries+1)
	if err := r.register(overflowID, ""); err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatalf("overflow registration error = %v, want explicit capacity failure", err)
	}
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != maxReaperEntries {
		t.Fatalf("reaper retained %d entries after overflow, want %d", len(entries), maxReaperEntries)
	}
	if entries[0].id != fmt.Sprintf("%064x", 1) || entries[len(entries)-1].id != fmt.Sprintf("%064x", maxReaperEntries) {
		t.Fatalf("overflow changed live entries: first=%q last=%q", entries[0].id, entries[len(entries)-1].id)
	}
	r.closeStdin()
}

func TestReaperRetirementDoesNotDeleteAtEOF(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")
	uid := strings.Repeat("7", 64)
	if err := r.register(uid, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.unregisterHandoff(uid, uid); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	entries, active, exited := len(r.entries), r.cmd != nil, r.exited
	r.mu.Unlock()
	if entries != 0 {
		t.Fatalf("retirement left %d entries", entries)
	}
	if active {
		r.closeStdin()
	}
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Fatal("retired reaper child did not exit")
		}
	}
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "rm --force "+uid) {
		t.Fatalf("retired entry was deleted at EOF: %q", data)
	}
}

func TestReaperRetirementDoesNotDeleteHandedOffEntryDuringReplay(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")
	handedOff := strings.Repeat("7", 64)
	remaining := strings.Repeat("8", 64)
	if err := r.register(handedOff, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.register(remaining, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.unregisterHandoff(handedOff, handedOff); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+remaining)
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "rm --force "+handedOff) {
		t.Fatalf("handed-off entry was deleted during replay: %q", data)
	}
}

func TestReaperCanReplayAfterAcknowledgedRetirement(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")
	handedOff := strings.Repeat("5", 64)
	remaining := strings.Repeat("6", 64)
	if err := r.register(handedOff, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.register(remaining, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.unregisterHandoff(handedOff, handedOff); err != nil {
		t.Fatal(err)
	}
	r.opMu.Lock()
	r.mu.Lock()
	err := r.respawnAndReplayLocked()
	r.mu.Unlock()
	r.opMu.Unlock()
	if err != nil {
		t.Fatalf("replay after acknowledged retirement: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+remaining)
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "rm --force "+handedOff) {
		t.Fatalf("handed-off entry was deleted during replay: %q", data)
	}
	if count := strings.Count(string(data), "rm --force "+remaining); count != 1 {
		t.Fatalf("remaining entry delete count = %d, want one after replay: %q", count, data)
	}
}

func TestReaperDockerGenerationRequiresImmutableID(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "docker")
	creation := "0123456789abcdef"
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo '      \"" + creationLabel + "\": \"" + creation + "\"'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "rm")
	if err := r.register("generation-guard", creation); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect generation-guard")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "rm --force") {
			t.Fatalf("generation-guarded entry fell back to name: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReaperFailsClosedWhenLockInodeIsReplaced(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("replace-lock", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	path := r.entries[0].lockPaths[0]
	r.mu.Unlock()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	time.Sleep(150 * time.Millisecond)
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "delete --force replace-lock") {
		t.Fatalf("reaper deleted through a replaced lock inode: %q", data)
	}
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
}

func TestReaperRespawnsAndReRegisters(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm")

	before := strings.Repeat("c", 64)
	after := strings.Repeat("d", 64)
	if err := r.register(before, ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Kill the reaper child; killing closes its stdin read side, so it
	// reaps what it knows, then the next register must respawn it.
	r.killForTest()

	if err := r.register(after, ""); err != nil {
		t.Fatalf("register after crash: %v", err)
	}
	r.closeStdin()

	waitForLogLines(t, logPath, "rm --force "+before, "rm --force "+after)
}

func TestReaperStopsOldChildBeforeReplay(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "rm")
	defer r.closeStdin()
	var starts atomic.Int32
	var firstPID int
	r.command = func() *exec.Cmd {
		starts.Add(1)
		return exec.Command("/bin/sh", "-c", "while IFS= read -r line; do case \"$line\" in R#containergo-retire:*|Q#containergo-quiesce:*) echo \"containergo-reaper-ack:${line#*:}\";; esac; done")
	}
	uid := strings.Repeat("9", 64)
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	r.mu.Lock()
	firstPID = r.cmd.Process.Pid
	r.mu.Unlock()

	r.opMu.Lock()
	r.mu.Lock()
	err := r.respawnAndReplayLocked()
	r.mu.Unlock()
	r.opMu.Unlock()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if starts.Load() != 2 {
		t.Fatalf("spawn count = %d, want old child retired and one replay child", starts.Load())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(firstPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("old reaper child %d was not terminated before replay", firstPID)
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, `sleep "$timeout"`) ||
		(!strings.Contains(reaperScript, "kill -9") && !strings.Contains(reaperScript, `kill "-$signal"`)) {
		t.Error("reaper script must bound each backend call with sleep/kill (no timeout(1))")
	}
	if strings.Contains(reaperScript, "kill -9 "+"-") {
		t.Error("reaper script must not signal a recyclable process-group ID")
	}
	if !strings.Contains(reaperScript, "ps_bin") || !strings.Contains(reaperScript, "-o lstart=") || !strings.Contains(reaperScript, "kill_owned_descendants") {
		t.Error("reaper script must verify positive child identity before cleanup")
	}
	// The creation label must be read as a structural JSON field, anchored
	// at line start on the quoted key, and compared for exact equality.
	if !strings.Contains(reaperScript, `s/^[[:space:]]*\"$key\"[[:space:]]*:`) {
		t.Error("reaper script must anchor the creation label match on the quoted key")
	}
	if !strings.Contains(reaperScript, `[ "$got" = "$creation" ]`) {
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
	r := newReaper(bin, "rm")
	r.spawnFailures = 2
	if err := r.register(strings.Repeat("e", 64), ""); err != nil {
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
