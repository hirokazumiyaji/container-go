//go:build !windows

package container

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestNameLockPathIsStableAcrossLaunchEnvironments(t *testing.T) {
	name := "lock-" + newContainerName()
	first, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	second, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath after launch environment change: %v", err)
	}
	if first != second {
		t.Fatalf("nameLockPath changed with launch environment: %q != %q", first, second)
	}
	if strings.Contains(filepath.Base(first), name) {
		t.Fatalf("lock filename %q contains the un-hashed name", filepath.Base(first))
	}
}

func TestNameLockPathUsesAccountDerivedDurableState(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	stateHome := filepath.Join(current.HomeDir, ".local", "state")
	if runtime.GOOS == "darwin" {
		stateHome = filepath.Join(current.HomeDir, "Library", "Application Support")
	}
	wantDir := filepath.Join(stateHome, "container-go", "locks")

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := nameLockPath("lock-" + newContainerName())
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(path); got != wantDir {
		t.Fatalf("lock directory = %q, want durable account state %q", got, wantDir)
	}
}

func TestNameLockRejectsSymlink(t *testing.T) {
	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do not lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := lockName(context.Background(), name); err == nil {
		t.Fatal("lockName followed a pre-existing symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do not lock" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestNameLockRejectsWrongPermissions(t *testing.T) {
	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lockName(context.Background(), name); err == nil {
		t.Fatal("lockName accepted a world-readable lock file")
	}
}

func TestNameLockReusesStaleFile(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("initial lock: %v", err)
	}
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	// The file is deliberately retained after unlock. It is stale as a
	// lock, but its inode remains the coordination point.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stale lock file missing: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("lock file size = %d, want zero", info.Size())
	}
	unlock, err = lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lock stale file: %v", err)
	}
	unlock()
}

func TestLockNameSerializesHolders(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock while held: err = %v, want deadline exceeded", err)
	}

	unlock()
	unlock2, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestLockNameWaitsForHistoricalLockBarriers(t *testing.T) {
	resolvers := []struct {
		name string
		path func(string) (string, error)
	}{
		{name: "legacy temp", path: legacyNameLockPath},
		{name: "transitional cache", path: transitionalNameLockPath},
	}
	for _, tc := range resolvers {
		t.Run(tc.name, func(t *testing.T) {
			name := "lock-" + newContainerName()
			path, err := tc.path(name)
			if err != nil {
				t.Fatal(err)
			}
			f, err := openNameLockPath(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				_ = f.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			})

			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			defer cancel()
			if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lockName with historical holder = %v, want deadline exceeded", err)
			}
		})
	}
}

func TestLockNameRejectsHeldPathReplacement(t *testing.T) {
	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, nameLockFilePerm); err != nil {
		t.Fatal(err)
	}
	moved := path + ".opened"
	var hookErr error
	hooks := &nameLockHooks{afterAcquire: func(got string) {
		if got != path || hookErr != nil {
			return
		}
		if err := os.Rename(path, moved); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(path, nil, nameLockFilePerm)
	}}
	_, err = lockNameWithHooks(context.Background(), name, hooks)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("lockName = %v, want replaced-inode refusal", err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("opened lock inode disappeared: %v", err)
	}
}

func TestLockNameHoldsProcessLocalBarrier(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}

	processNameLocks.Lock()
	held, ok := processNameLocks.byName[name]
	references := 0
	if ok {
		references = held.references
	}
	processNameLocks.Unlock()
	if !ok || references != 1 {
		unlock()
		t.Fatalf("process lock = present:%v references:%d, want one held reference", ok, references)
	}

	unlock()
	processNameLocks.Lock()
	_, leaked := processNameLocks.byName[name]
	processNameLocks.Unlock()
	if leaked {
		t.Fatal("process lock entry remained after unlock")
	}
}

func TestCanonicalLockPathIsAbsoluteAndMigrationPathsDeduplicate(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	canonical, err := canonicalLockPath(filepath.Join(alias, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(canonical) {
		t.Fatalf("canonical path = %q, want absolute", canonical)
	}
	resolved := resolvedNameLocks{
		legacy:       canonical,
		transitional: canonical,
		state:        filepath.Join(dir, "durable.lock"),
	}
	paths := resolved.ordered()
	if len(paths) != 2 || paths[0] != canonical || paths[1] != resolved.state {
		t.Fatalf("ordered paths = %q, want two canonical unique barriers", paths)
	}
}

func TestProcessNameLockIsContextAwareAndPerName(t *testing.T) {
	firstName := "lock-" + newContainerName()
	secondName := "lock-" + newContainerName()
	unlockFirst, err := acquireProcessNameLock(context.Background(), firstName)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockFirst()

	unlockSecond, err := acquireProcessNameLock(context.Background(), secondName)
	if err != nil {
		t.Fatalf("different name did not acquire independently: %v", err)
	}
	unlockSecond()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireProcessNameLock(ctx, firstName); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-name waiter = %v, want deadline exceeded", err)
	}

	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := acquireProcessNameLock(canceled, "lock-"+newContainerName()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition = %v, want context canceled", err)
	}
}

func TestTerminateWaitsForNameLockBeforeInspecting(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	// Another process holds the name: this handle's inspect-then-delete
	// must not start until it is released, so a bounded context fails
	// before any backend call.
	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = ctr.Terminate(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Terminate = %v, want lock failure", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 while the name is locked elsewhere", r.deleteCalls)
	}
}

func TestTerminateContainerBoundsNameLockWait(t *testing.T) {
	oldTimeout := terminateTimeout
	terminateTimeout = 100 * time.Millisecond
	t.Cleanup(func() { terminateTimeout = oldTimeout })

	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	start := time.Now()
	err = TerminateContainer(ctr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TerminateContainer = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("TerminateContainer took %v, want bounded wait", elapsed)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", r.deleteCalls)
	}
}

func TestPruneAppleWaitsForNameLockBeforeInspect(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &pruneSafetyRunner{
		list:     []pruneFixtureContainer{pruneFixture(name, creation, string(StateStopped), "", true)},
		inspects: []pruneFixtureContainer{pruneFixture(name, creation, string(StateStopped), "", true)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = pruneWith(ctx, r, appleEngine{})
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Prune = %v, want lock failure", err)
	}
	if r.callCount("inspect") != 0 || len(r.deleted) != 0 {
		t.Fatalf("inspect calls = %d, deleted = %v; backend touched while locked", r.callCount("inspect"), r.deleted)
	}
}

func TestRunAppleCreateTakesNameLock(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	f := newTestRunner()
	f.imagePresent = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = Run(ctx, "redis:7-alpine", WithName(name), withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Run = %v, want create lock failure", err)
	}
	if f.callWith("run") != nil {
		t.Error("run was issued while name lock was held")
	}
}

func TestDockerCreateDoesNotWaitForAppleNameLock(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	f := newTestRunner()
	cfg := &config{name: name, runner: f, eng: dockerEngine{}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, attempted, err := runCreateLocked(ctx, cfg, "run"); err != nil || !attempted {
		t.Fatalf("Docker create = attempted:%v err:%v", attempted, err)
	}
	if f.callWith("run") == nil {
		t.Fatal("Docker run was incorrectly coupled to the Apple name lock")
	}
}

func TestDockerFailedCreateCleanupDoesNotWaitForAppleNameLock(t *testing.T) {
	name := "myctr"
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	oldTimeout := cleanupFailedCreateTimeout
	cleanupFailedCreateTimeout = 250 * time.Millisecond
	t.Cleanup(func() { cleanupFailedCreateTimeout = oldTimeout })

	const creation = "aaaaaaaaaaaaaaaa"
	inspectJSON, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	fakeLockDir := t.TempDir()
	fakeLock := filepath.Join(fakeLockDir, "lockf")
	if err := os.WriteFile(fakeLock, []byte("#!/bin/sh\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeLockDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := &dockerRunner{
		fakeRunner:      newTestRunner(),
		inspectJSON:     inspectJSON,
		bindRunIdentity: true,
		creation:        creation,
	}
	cfg := &config{name: name, runner: d, eng: dockerEngine{}, creation: creation}
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "entrypoint failed"}
	if err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr); err != nil {
		t.Fatalf("Docker cleanup waited for Apple lock: %v", err)
	}
	var rm []string
	for _, call := range d.calls {
		if len(call) > 0 && call[0] == "rm" {
			rm = call
			break
		}
	}
	if rm == nil || rm[len(rm)-1] != dockerFixtureID {
		t.Fatalf("rm = %v, want immutable ID %s", rm, dockerFixtureID)
	}
}

const nameLockHelperEnv = "CONTAINERGO_NAMELOCK_HELPER"

func TestNameLockExcludesAnotherProcessAcrossTempDirs(t *testing.T) {
	name := "lock-" + newContainerName()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNameLockHelperProcess$")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	// Different launch environments must not select a different lock
	// inode for the same user and name.
	cmd.Env = append(os.Environ(),
		nameLockHelperEnv+"=1",
		"CONTAINERGO_NAMELOCK_NAME="+name,
		"TMPDIR="+t.TempDir(),
		"HOME="+t.TempDir(),
		"XDG_CACHE_HOME="+t.TempDir(),
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		if strings.Contains(line, "CONTAINERGO_LOCKED") {
			break
		}
		if readErr != nil {
			t.Fatalf("helper output = %q, err = %v", line, readErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockName while helper holds lock = %v, want deadline exceeded", err)
	}

	// Closing stdin is the helper's deterministic release signal.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName after helper release: %v", err)
	}
	unlock()
}

// TestNameLockHelperProcess is launched as a separate test binary by
// TestNameLockExcludesAnotherProcessAcrossTempDirs. It holds the lock
// until stdin closes.
func TestNameLockHelperProcess(t *testing.T) {
	if os.Getenv(nameLockHelperEnv) != "1" {
		return
	}
	name := os.Getenv("CONTAINERGO_NAMELOCK_NAME")
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fmt.Fprintln(os.Stdout, "CONTAINERGO_LOCKED")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
