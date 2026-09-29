package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func requireReaperLockf(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("watchdog reaper is unavailable on Windows")
	}
	if _, err := exec.LookPath("lockf"); err != nil {
		t.Skipf("lockf is unavailable: %v", err)
	}
}

// writeReaperStub creates a fake `container` binary that logs its argv.
func writeReaperStub(t *testing.T) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binPath = filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then echo '  \"" + creationLabel + "\": \"0123456789abcdef\",'; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
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
	requireReaperLockf(t)
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("ctr-one", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register("ctr-two", "0123456789abcdef"); err != nil {
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
	requireReaperLockf(t)
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("before-crash", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Kill the reaper child; killing closes its stdin read side, so it
	// reaps what it knows, then the next register must respawn it.
	r.killForTest()

	if err := r.register("after-crash", "0123456789abcdef"); err != nil {
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
	if !strings.Contains(reaperScript, `[ "$got" = "$creation" ] || exit 0`) {
		t.Error("reaper script must compare the extracted generation exactly")
	}
	if !strings.Contains(reaperScript, `command -v lockf`) || !strings.Contains(reaperScript, `"$lockf_bin" -k -n -t 30 -w`) ||
		!strings.Contains(reaperScript, `identities_valid`) {
		t.Error("reaper script must hold all stable name locks across inspect and delete")
	}
	for _, variable := range []string{"lock1", "lock2", "lock3", "lock4"} {
		if !strings.Contains(reaperScript, variable) {
			t.Errorf("reaper script does not carry %s", variable)
		}
	}
}

func TestReaperRejectsLegacyUnlockedNameEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("watchdog reaper is unavailable on Windows")
	}
	bin, logPath := writeReaperStub(t)
	cmd := exec.Command("/bin/sh", "-c", reaperScript, "legacy-reaper-test", bin, "delete", creationLabel)
	cmd.Stdin = strings.NewReader("legacy-name\n")
	if err := cmd.Run(); err != nil {
		t.Fatalf("legacy protocol probe: %v", err)
	}
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force legacy-name") {
		t.Fatalf("current reaper processed an old unlocked entry: %q", data)
	}
}

func waitReaperExitForTest(t *testing.T, r *reaper) {
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

func TestReaperFailsClosedWhenLockfUnavailable(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	t.Setenv("PATH", t.TempDir())
	r := newReaper(bin, "delete")
	if err := r.register("ctr", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitReaperExitForTest(t, r)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force ctr") {
		t.Fatalf("reaper deleted without lockf: %q", data)
	}
}

func TestReaperDoesNotFollowReplacedLockSymlink(t *testing.T) {
	requireReaperLockf(t)
	bin, logPath := writeReaperStub(t)
	name := "reaper-symlink-" + newContainerName()
	r := newReaper(bin, "delete")
	if err := r.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitReaperExitForTest(t, r)
	})
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "other-lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r.closeStdin()
	waitReaperExitForTest(t, r)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force "+name) {
		t.Fatalf("reaper followed a replaced lock symlink: %q", data)
	}
}

type reaperPruneInterleaveRunner struct {
	mu            sync.Mutex
	name          string
	inspectCalled chan struct{}
	deleted       []string
}

func (r *reaperPruneInterleaveRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "ls":
		return mustPruneJSON([]pruneFixtureContainer{
			pruneFixture(r.name, "aaaaaaaaaaaaaaaa", string(StateStopped), "", true),
		}), nil, nil
	case "inspect":
		select {
		case r.inspectCalled <- struct{}{}:
		default:
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "container not found"}
	case "delete", "rm":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func waitForFile(t *testing.T, path string) {
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

func TestReaperUsesMigrationBarrierOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("watchdog reaper is unavailable on Windows")
	}
	name := "reaper-order-" + newContainerName()
	creation := "0123456789abcdef"
	paths, _, err := reaperNameLockSet(name)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "lockf.log")
	lockfPath := filepath.Join(dir, "lockf")
	script := `#!/bin/sh
printf '%s\n' "$6" >> ` + logPath + `
shift 6
exec "$@"
`
	if err := os.WriteFile(lockfPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin, deleteLog := writeReaperStub(t)
	r := newReaper(bin, "delete")
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, deleteLog, "delete --force "+name)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("lockf log: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(got) != len(paths) {
		t.Fatalf("lockf calls = %q, want %d barriers", got, len(paths))
	}
	for i, want := range paths {
		if got[i] != want {
			t.Fatalf("lockf call %d = %q, want %q", i+1, got[i], want)
		}
	}
}

func TestReaperAppleDeleteCoordinatesWithPrune(t *testing.T) {
	requireReaperLockf(t)
	name := "reaper-prune-" + newContainerName()
	const generation = "aaaaaaaaaaaaaaaa"
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	inspectStarted := filepath.Join(dir, "inspect-started")
	releaseInspect := filepath.Join(dir, "release-inspect")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  : > " + inspectStarted + "\n" +
		"  while [ ! -e " + releaseInspect + " ]; do sleep 0.05; done\n" +
		"  printf '    \"" + creationLabel + "\": \"" + generation + "\"\n'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	t.Cleanup(func() {
		_ = os.WriteFile(releaseInspect, nil, 0o600)
		r.closeStdin()
		waitReaperExitForTest(t, r)
	})
	if err := r.register(name, generation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForFile(t, inspectStarted)

	pruneRunner := &reaperPruneInterleaveRunner{name: name, inspectCalled: make(chan struct{}, 1)}
	pruneDone := make(chan error, 1)
	go func() {
		_, err := pruneWith(context.Background(), pruneRunner, appleEngine{})
		pruneDone <- err
	}()
	select {
	case <-pruneRunner.inspectCalled:
		t.Fatal("prune inspected while the reaper held the name lock")
	case <-time.After(250 * time.Millisecond):
	}
	if err := os.WriteFile(releaseInspect, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLogLines(t, logPath, "delete --force "+name)
	select {
	case err := <-pruneDone:
		if err != nil {
			t.Fatalf("prune after reaper release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prune did not finish after reaper release")
	}
}

func TestBreQuoteEscapesLabelKey(t *testing.T) {
	if got := breQuote("com.github.x-y"); got != `com\.github\.x-y` {
		t.Errorf("breQuote = %q", got)
	}
}

func TestReaperSpawnFailuresResetOnSuccess(t *testing.T) {
	requireReaperLockf(t)
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.spawnFailures = 2
	if err := r.register("ok", "0123456789abcdef"); err != nil {
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
	requireReaperLockf(t)
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
	requireReaperLockf(t)
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
	requireReaperLockf(t)
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
	if err := r.register(uid, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "rm --force ctr") {
		t.Fatalf("reaper deleted by name despite an immutable Id: %q", data)
	}
}

func leaseEntrySnapshot(r *reaper) reaperEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == 0 {
		return reaperEntry{}
	}
	entry := r.entries[0]
	entry.lockPaths = append([]string(nil), entry.lockPaths...)
	entry.lockIdentities = append([]string(nil), entry.lockIdentities...)
	entry.leaseHolds = append([]string(nil), entry.leaseHolds...)
	return entry
}

func assertLeasePathsAbsent(t *testing.T, paths ...[]string) {
	t.Helper()
	for _, group := range paths {
		for _, path := range group {
			// The maintenance inode is shared by every name in a
			// namespace. A fixed maintenance lease may legitimately remain
			// while another reaper owns a hold; each entry's hold is still
			// checked below.
			if strings.HasSuffix(filepath.Base(path), nameLockMaintenanceFile+nameLockLeaseSuffix) {
				continue
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lease path %s remains after reclamation (stat error=%v)", path, err)
			}
		}
	}
}

func reaperLeaseHoldCount(t *testing.T, raw string) int {
	t.Helper()
	leases, err := reaperLeasePaths(raw)
	if err != nil {
		return 0
	}
	count := 0
	for _, lease := range leases {
		if _, fixed, ok := reaperLeaseRawName(filepath.Base(lease)); ok && !fixed {
			count++
		}
	}
	return count
}

func reaperRawBarrierPaths(t *testing.T, name string) []string {
	t.Helper()
	legacy, err := rawLegacyNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := rawTransitionalNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	state, err := rawNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return []string{legacy, cache, filepath.Join(filepath.Dir(state), nameLockMaintenanceFile), state}
}

func TestReaperReclaimsLeasesAfterEOF(t *testing.T) {
	requireReaperLockf(t)
	bin, _ := writeReaperStub(t)
	name := "lease-eof-" + newContainerName()
	r := newReaper(bin, "delete")
	if err := r.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	entry := leaseEntrySnapshot(r)
	r.closeStdin()
	waitReaperExitForTest(t, r)
	assertLeasePathsAbsent(t, entry.lockPaths, entry.leaseHolds)
}

func TestReaperShellReclaimsLeasesAfterEOF(t *testing.T) {
	requireReaperLockf(t)
	bin, logPath := writeReaperStub(t)
	name := "lease-shell-eof-" + newContainerName()
	paths, identities, holds, err := reaperNameLockSetForReaper(name)
	if err != nil {
		t.Fatal(err)
	}
	fields := []string{name, "0123456789abcdef"}
	for i, path := range paths {
		fields = append(fields, path, identities[i])
	}
	fields = append(fields, holds...)
	cmd := exec.Command("/bin/sh", "-c", reaperScript, "shell-lease-test", bin, "delete", creationLabel)
	cmd.Stdin = strings.NewReader(strings.Join(fields, "\t") + "\n")
	if err := cmd.Run(); err != nil {
		t.Fatalf("shell reaper: %v", err)
	}
	waitForLogLines(t, logPath, "delete --force "+name)
	assertLeasePathsAbsent(t, paths, holds)
}

func TestReaperReclaimsLeasesAfterRegistrationFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("watchdog reaper is unavailable on Windows")
	}
	bin, _ := writeReaperStub(t)
	name := "lease-register-failure-" + newContainerName()
	r := newReaper(bin, "delete")
	raws := reaperRawBarrierPaths(t, name)
	beforeHolds := make([]int, len(raws))
	for i, raw := range raws {
		beforeHolds[i] = reaperLeaseHoldCount(t, raw)
	}
	r.spawnFailures = maxReaperSpawnFailures
	if err := r.register(name, "0123456789abcdef"); err == nil {
		t.Fatal("register succeeded despite exhausted spawn attempts")
	}
	if got := len(r.entries); got != 0 {
		t.Fatalf("entries after failed registration = %d, want 0", got)
	}
	for i, raw := range raws {
		if i == 2 { // shared maintenance lease
			if got := reaperLeaseHoldCount(t, raw); got > beforeHolds[i] {
				t.Fatalf("maintenance lease hold count grew after failed registration: %d -> %d", beforeHolds[i], got)
			}
			continue
		}
		if reaperLeaseExists(raw) {
			t.Fatalf("lease remains for failed registration at %s", raw)
		}
	}
}

func TestReaperLeaseOwnershipIsReferenceCounted(t *testing.T) {
	requireReaperLockf(t)
	bin, _ := writeReaperStub(t)
	name := "lease-refcount-" + newContainerName()
	first := newReaper(bin, "delete")
	second := newReaper(bin, "delete")
	if err := first.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := second.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("second register: %v", err)
	}
	firstEntry := leaseEntrySnapshot(first)
	secondEntry := leaseEntrySnapshot(second)
	if err := first.unregister(name, "0123456789abcdef", true); err != nil {
		t.Fatalf("first unregister: %v", err)
	}
	if _, err := os.Lstat(firstEntry.lockPaths[3]); err != nil {
		t.Fatalf("shared fixed lease removed after first unregister: %v", err)
	}
	if err := second.unregister(name, "0123456789abcdef", true); err != nil {
		t.Fatalf("second unregister: %v", err)
	}
	assertLeasePathsAbsent(t, secondEntry.lockPaths, secondEntry.leaseHolds)
	first.closeStdin()
	second.closeStdin()
}

type leaseTestExternalRunner struct {
	inner  cli.Runner
	binary string
}

func (r *leaseTestExternalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return r.inner.Run(ctx, args...)
}
func (r *leaseTestExternalRunner) External() bool         { return true }
func (r *leaseTestExternalRunner) ExternalBinary() string { return r.binary }

func TestTerminateReclaimsReaperLease(t *testing.T) {
	requireReaperLockf(t)
	bin, _ := writeReaperStub(t)
	name := "lease-terminate-" + newContainerName()
	creation := "0123456789abcdef"
	r := newReaper(bin, "delete")
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	entry := leaseEntrySnapshot(r)
	globalReapersMu.Lock()
	globalReapers[bin] = r
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		r.closeStdin()
	})

	backend := &genRunner{
		fakeRunner:  newTestRunner(),
		inspectJSON: creationInspectJSON(name, creation),
	}
	ctr := &Container{
		id: name, creation: creation, runner: &leaseTestExternalRunner{inner: backend, binary: bin}, eng: appleEngine{},
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	assertLeasePathsAbsent(t, entry.lockPaths, entry.leaseHolds)
}
