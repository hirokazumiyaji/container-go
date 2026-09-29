//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func writeReaperGenerationStub(t *testing.T, generations map[string]string) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binPath = filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	script += "if [ \"$1\" = inspect ]; then\n  case \"$2\" in\n"
	for name, creation := range generations {
		script += fmt.Sprintf("    %s) printf '    %q: \"%%s\"\\n' %q ;;\n", name, creationLabel, creation)
	}
	script += "  esac\nfi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, logPath
}

func TestReaperDeletesRegisteredContainersOnEOF(t *testing.T) {
	nameOne := "reaper-one-" + newContainerName()
	nameTwo := "reaper-two-" + newContainerName()
	bin, logPath := writeReaperGenerationStub(t, map[string]string{
		nameOne: "0123456789abcdef",
		nameTwo: "fedcba9876543210",
	})
	r := newReaper(bin, "delete")

	if err := r.register(nameOne, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register(nameTwo, "fedcba9876543210"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Closing stdin is what the reaper sees when the parent process
	// dies, however it dies.
	r.closeStdin()

	waitForLogLines(t, logPath, "delete --force "+nameOne, "delete --force "+nameTwo)
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
	if err := r.register("generationless", ""); err == nil {
		t.Error("generation-less name delete: want error")
	}
}

func TestReaperRespawnsAndReRegisters(t *testing.T) {
	before := "reaper-before-" + newContainerName()
	after := "reaper-after-" + newContainerName()
	bin, logPath := writeReaperGenerationStub(t, map[string]string{
		before: "0123456789abcdef",
		after:  "fedcba9876543210",
	})
	r := newReaper(bin, "delete")

	if err := r.register(before, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Kill the reaper child; killing closes its stdin read side, so it
	// reaps what it knows, then the next register must respawn it.
	r.killForTest()

	if err := r.register(after, "fedcba9876543210"); err != nil {
		t.Fatalf("register after crash: %v", err)
	}
	r.closeStdin()

	waitForLogLines(t, logPath, "delete --force "+before, "delete --force "+after)
}
func TestReaperFailsClosedWhenNameLockHelperIsUnavailable(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	t.Setenv("PATH", t.TempDir())
	r := newReaper(bin, "delete")
	name := "reaper-no-helper-" + newContainerName()
	if err := r.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForReaperExitForTest(t, r)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force "+name) {
		t.Fatalf("reaper deleted without lockf: %q", data)
	}
}

func TestReaperDoesNotFollowReplacedLockSymlink(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	name := "reaper-symlink-" + newContainerName()
	lockPaths, err := reaperNameLockPaths(name)
	if err != nil {
		t.Fatal(err)
	}
	path := lockPaths[len(lockPaths)-1].path
	r := newReaper(bin, "delete")
	if err := r.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExitForTest(t, r)
	})
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
	waitForReaperExitForTest(t, r)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force "+name) {
		t.Fatalf("reaper followed a replaced lock symlink: %q", data)
	}
}

func TestReaperRejectsReplacedLockInode(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	name := "reaper-inode-" + newContainerName()
	targets, err := reaperNameLockPaths(name)
	if err != nil {
		t.Fatal(err)
	}
	path := targets[len(targets)-1].path
	r := newReaper(bin, "delete")
	if err := r.register(name, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, nameLockFilePerm); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	waitForReaperExitForTest(t, r)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force "+name) {
		t.Fatalf("reaper deleted through a replaced lock inode: %q", data)
	}
}

func TestReaperSelectsPortableLockHelperByOS(t *testing.T) {
	if got := reaperLockHelperForOS("darwin"); got != "lockf" {
		t.Errorf("darwin helper = %q, want lockf", got)
	}
	if got := reaperLockHelperForOS("linux"); got != "flock" {
		t.Errorf("linux helper = %q, want flock", got)
	}
}

func TestReaperDoesNotStageInspectOutputOnDisk(t *testing.T) {
	if strings.Contains(reaperScript, "mktemp") || strings.Contains(reaperScript, "inspect_fields=$(mktemp") {
		t.Fatal("reaper stages inspect output instead of streaming it through the field filter")
	}
	for _, required := range []string{"sed -n", "inspect_fields", "creation=", "id="} {
		if !strings.Contains(reaperScript, required) {
			t.Errorf("reaper script is missing streaming field projection %q", required)
		}
	}
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, `sleep "$run_timeout"`) ||
		!strings.Contains(reaperScript, `kill -KILL -"$command_group"`) ||
		!strings.Contains(reaperScript, `kill -TERM "$timer_pid"`) {
		t.Error("reaper script must bound each backend call with a supervised process group")
	}
	if strings.Contains(reaperScript, "kill_backend_tree") ||
		strings.Contains(reaperScript, "process_identity") ||
		strings.Contains(reaperScript, "lstart=") {
		t.Error("reaper script must not signal a descendant PID after a lookup")
	}
	// The creation label must be read as a structural JSON field, anchored
	// at line start on the quoted key, and compared for exact equality.
	if !strings.Contains(reaperScript, `s/^[[:space:]]*\"$key\"[[:space:]]*:`) {
		t.Error("reaper script must anchor the creation label match on the quoted key")
	}
	if !strings.Contains(reaperScript, `[ "$got" = "$creation" ]`) {
		t.Error("reaper script must compare the extracted generation exactly")
	}
	for _, required := range []string{
		`lockf)`,
		`lock_flag1=-k`,
		`lock_flag2=-t`,
		`flock)`,
		`lock_flag1=-x`,
		`lock_flag2=-w`,
		`"$lock_bin" "$lock_flag1" "$lock_flag2" "$timeout" "$lock"`,
		`REAPER_LOCKED_COMMAND="$locked_command"`,
		`run_locked "$id" "$creation" "$@"`,
	} {
		if !strings.Contains(reaperScript, required) {
			t.Errorf("reaper script is missing %q", required)
		}
	}
	if strings.Contains(reaperScript, `"$lockf_bin" -k -w`) {
		t.Error("reaper script must not use the non-portable lockf -w flag")
	}
}

func TestReaperUsesPortableLockHelperFlags(t *testing.T) {
	helper := reaperLockHelper()
	flag1, flag2 := "-x", "-w"
	if helper == "lockf" {
		flag1, flag2 = "-k", "-t"
	}

	dir := t.TempDir()
	argsLog := filepath.Join(dir, "lock-args.log")
	helperPath := filepath.Join(dir, helper)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" "$2" "$3" "$4" >> %q
[ "$1" = %q ] || exit 91
[ "$2" = %q ] || exit 92
[ "$3" = "30" ] || exit 93
shift 4
exec "$@"
`, argsLog, flag1, flag2)
	if err := os.WriteFile(helperPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	name := "reaper-lock-" + newContainerName()
	creation := "0123456789abcdef"
	bin, logPath := writeReaperGenerationStub(t, map[string]string{name: creation})
	lockPaths, err := reaperNameLockPaths(name)
	if err != nil {
		t.Fatal(err)
	}

	r := newReaper(bin, "delete")
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExitForTest(t, r)
	})
	if err := r.register(name, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForReaperExitForTest(t, r)
	waitForLogLines(t, logPath, "delete --force "+name)

	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("%s helper was not called: %v", helper, err)
	}
	args := strings.Split(strings.TrimSuffix(string(argsData), "\n"), "\n")
	if len(args) != len(lockPaths)*4 {
		t.Fatalf("%s args = %q, want %d four-field calls", helper, args, len(lockPaths))
	}
	for i, path := range lockPaths {
		want := []string{flag1, flag2, "30", path.path}
		if got := args[i*4 : i*4+4]; !slices.Equal(got, want) {
			t.Fatalf("%s call %d = %q, want %q", helper, i+1, got, want)
		}
	}
}

func TestBreQuoteEscapesLabelKey(t *testing.T) {
	if got := breQuote("com.github.x-y"); got != `com\.github\.x-y` {
		t.Errorf("breQuote = %q", got)
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

func (w *blockingReaperWriter) Close() error {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
	return nil
}

func TestReaperHungInspectDoesNotBlockLaterEntries(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	first := "reaper-hung-" + newContainerName()
	later := "reaper-later-" + newContainerName()
	bin := filepath.Join(dir, "container")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
if [ "$1" = inspect ] && [ "$2" = %q ]; then
  sleep 30
  exit 0
fi
if [ "$1" = inspect ] && [ "$2" = %q ]; then
  echo '  "%s": "0123456789abcdef"'
fi
`, logPath, first, later, creationLabel)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	r.timeoutSeconds = 1
	r.inspectTimeoutSeconds = 1
	if err := r.register(first, "0123456789abcdef"); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := r.register(later, "0123456789abcdef"); err != nil {
		t.Fatalf("register later: %v", err)
	}
	r.closeStdin()
	waitForReaperExitForTest(t, r)
	waitForLogLines(t, logPath, "inspect "+first, "inspect "+later, "delete --force "+later)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force "+first) {
		t.Fatalf("timed-out first entry was deleted: %q", data)
	}
}

func TestReaperSuccessfulOperationCancelsTimeoutProcessGroup(t *testing.T) {
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	dir := t.TempDir()
	sleepLog := filepath.Join(dir, "sleep-pids.log")
	timerStarted := filepath.Join(dir, "timer-started")
	sleepPath := filepath.Join(dir, "sleep")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$$\" >> %q\n: > %q\nexec %q \"$@\"\n", sleepLog, timerStarted, realSleep)
	if err := os.WriteFile(sleepPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bin := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	backendScript := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nwhile [ ! -e %q ]; do /bin/sleep 0.01; done\n", logPath, timerStarted)
	if err := os.WriteFile(bin, []byte(backendScript), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "rm")
	r.timeoutSeconds = 1
	uid := strings.Repeat("ab", 32)
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)

	var pids []int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(sleepLog)
		for _, field := range strings.Fields(string(data)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
		if len(pids) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pids) == 0 {
		t.Fatal("reaper did not start its timeout helper")
	}

	// The backend completed, so the timer must be canceled immediately. If
	// its sleep child survives, it can later run the timeout path against a
	// recycled backend PID and signal an unrelated process.
	deadline = time.Now().Add(500 * time.Millisecond)
	for _, pid := range pids {
		for time.Now().Before(deadline) {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := syscall.Kill(pid, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("timeout helper pid %d survived successful operation: %v", pid, err)
		}
	}
	waitForReaperExitForTest(t, r)
}

func TestReaperRegistrationHonorsContextWhileWriterBlocks(t *testing.T) {
	writer := &blockingReaperWriter{started: make(chan struct{}), release: make(chan struct{})}
	r := &reaper{binary: "docker", subcommand: "rm", stdin: writer}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	uid := strings.Repeat("ab", 32)
	started := make(chan error, 1)
	go func() { started <- r.registerContext(ctx, uid, "") }()
	select {
	case err := <-started:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("registerContext error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("registerContext remained blocked on a writer")
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("blocking writer was not reached")
	}
	_ = writer.Close()
}

func TestReaperSpawnFailuresResetOnSuccess(t *testing.T) {
	requirePOSIXShell(t)
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
	guarded := "reaper-guarded-" + newContainerName()
	stale := "reaper-stale-" + newContainerName()
	r := newReaper(binPath, "delete")
	if err := r.register(guarded, "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register(stale, "ffffffffffffffff"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force "+guarded)
	// Stale generation must not be deleted; poll briefly to confirm absence.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force "+stale) {
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
	name := "reaper-association-" + newContainerName()
	r := newReaper(binPath, "delete")
	if err := r.register(name, oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect "+name)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force "+name) {
			t.Fatalf("reaper deleted on label value collision: %q", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReaperAllowsGenerationlessImmutableID(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	uid := strings.Repeat("cd", 32)
	r := newReaper(bin, "rm")
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("register immutable ID: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "inspect ") {
		t.Fatalf("immutable ID unexpectedly inspected: %q", data)
	}
}

func TestReaperDockerGenerationBearingIDUsesOnlyImmutableTarget(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "docker")
	uid := strings.Repeat("ab", 32)
	creation := "0123456789abcdef"
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
if [ "$1" = inspect ]; then
  echo '    "Id": "%s",'
  echo '      "%s": "%s"'
fi
`, logPath, uid, creationLabel, creation)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Docker's full immutable ID never enters the Apple name-lock path,
	// but a supplied generation is still verified before deletion.
	r := newReaper(bin, "rm")
	if err := r.register(uid, creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(r.entries) != 1 || len(r.entries[0].lockPaths) != 0 || r.entries[0].creation != creation {
		t.Fatalf("Docker entry acquired Apple metadata: %+v", r.entries)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect "+uid, "rm --force "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "rm --force ctr") {
		t.Fatalf("Docker reaper fell back to a name target: %q", data)
	}
}

func TestReaperDockerDoesNotRequireAppleLockHelper(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	helperDir := t.TempDir()
	if err := os.Symlink("/bin/sleep", filepath.Join(helperDir, "sleep")); err != nil {
		t.Skipf("sleep helper unavailable: %v", err)
	}
	t.Setenv("PATH", helperDir)
	uid := strings.Repeat("ef", 32)
	r := newReaper(bin, "rm")
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
}

type reaperInterleaveRunner struct {
	mu             sync.Mutex
	calls          []string
	name           string
	inspectEntered chan struct{}
}

func (r *reaperInterleaveRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(args, " "))
	r.mu.Unlock()

	switch args[0] {
	case "ls":
		return mustPruneJSON([]pruneFixtureContainer{
			pruneFixture(r.name, "aaaaaaaaaaaaaaaa", string(StateStopped), "", true),
		}), nil, nil
	case "inspect":
		select {
		case r.inspectEntered <- struct{}{}:
		default:
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "container not found"}
	case "delete", "rm":
		return nil, nil, nil
	case "run":
		return []byte("replacement\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func waitForReaperPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForReaperExitForTest(t *testing.T, r *reaper) {
	t.Helper()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("reaper child did not exit")
	}
}

// TestReaperAppleDeleteCoordinatesWithPruneAndCreate reproduces the
// inspect/delete window that motivated the name lock. The reaper pauses
// while holding the lock; prune must not inspect, and a later create must
// wait until the reaper has deleted the old generation. A replacement is
// therefore never a target of the reaper's name delete.
func TestReaperAppleDeleteCoordinatesWithPruneAndCreate(t *testing.T) {
	name := "reaper-interleave-" + newContainerName()
	const oldCreation = "aaaaaaaaaaaaaaaa"
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
		"  printf '    \"" + creationLabel + "\": \"" + oldCreation + "\"\\n'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	t.Cleanup(func() {
		_ = os.WriteFile(releaseInspect, nil, 0o600)
		r.closeStdin()
		waitForReaperExitForTest(t, r)
	})
	if err := r.register(name, oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForReaperPath(t, inspectStarted)

	pruneRunner := &reaperInterleaveRunner{name: name, inspectEntered: make(chan struct{}, 1)}
	pruneDone := make(chan error, 1)
	go func() {
		_, err := pruneWith(context.Background(), pruneRunner, appleEngine{})
		pruneDone <- err
	}()

	select {
	case <-pruneRunner.inspectEntered:
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

	createRunner := &reaperInterleaveRunner{name: name}
	cfg := &config{name: name, runner: createRunner, eng: appleEngine{}}
	if _, attempted, err := runCreateLocked(context.Background(), cfg, "run"); err != nil || !attempted {
		t.Fatalf("replacement create = attempted:%v err:%v", attempted, err)
	}
	createRunner.mu.Lock()
	gotRun := len(createRunner.calls) == 1 && createRunner.calls[0] == "run"
	calls := append([]string(nil), createRunner.calls...)
	createRunner.mu.Unlock()
	if !gotRun {
		t.Fatalf("replacement create calls = %v, want one run", calls)
	}
}
