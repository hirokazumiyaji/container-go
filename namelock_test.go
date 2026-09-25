//go:build !windows

package container

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

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

func TestNameLockPathIsStableAcrossTempDirs(t *testing.T) {
	name := "lock-" + newContainerName()
	first, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	second, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath after TMPDIR change: %v", err)
	}
	if first != second {
		t.Fatalf("nameLockPath changed with historical lock environment: %q != %q", first, second)
	}
	if strings.Contains(filepath.Base(first), name) {
		t.Fatalf("lock filename %q contains the un-hashed name", filepath.Base(first))
	}
}

func TestNameLockPathUsesDurableStateNamespace(t *testing.T) {
	stateHome := t.TempDir()
	cacheHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}
	resolvedState, err := filepath.EvalSymlinks(stateHome)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCache, err := filepath.EvalSymlinks(cacheHome)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(resolvedState, "container-go", "locks")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("durable lock directory = %q, want %q", filepath.Dir(path), wantDir)
	}
	if strings.HasPrefix(filepath.Dir(path), resolvedCache) {
		t.Fatalf("durable lock path %q unexpectedly uses cache %q", path, resolvedCache)
	}
}

func TestDefaultStateDirRejectsRelativeNamespaceByFallback(t *testing.T) {
	home := t.TempDir()
	got := defaultStateDir(home, "relative/state")
	var want string
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support")
	} else {
		want = filepath.Join(home, ".local", "state")
	}
	if got != want {
		t.Fatalf("defaultStateDir = %q, want %q", got, want)
	}
}

func TestPrepareUserBaseRejectsWritableParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareUserBase(filepath.Join(parent, "state"), "test state"); err == nil ||
		!strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("prepareUserBase error = %v, want unsafe-parent error", err)
	}
}

func TestPrepareUserBaseRejectsSymlinkRoot(t *testing.T) {
	real := t.TempDir()
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "state-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := prepareUserBase(alias, "test state"); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("prepareUserBase error = %v, want symlink rejection", err)
	}
}

func TestNameLockFailsClosedWhenTransitionalCacheUnavailable(t *testing.T) {
	name := "lock-" + newContainerName()
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	_, err := lockName(context.Background(), name)
	if !errors.Is(err, ErrNameLockCompatibility) ||
		!strings.Contains(err.Error(), "name lock compatibility") ||
		!strings.Contains(err.Error(), "transitional user cache directory") {
		t.Fatalf("lockName error = %v, want transitional compatibility failure", err)
	}
	legacyPath, pathErr := legacyNameLockPath(name)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Lstat(legacyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("legacy lock was opened before compatibility paths resolved: %v", statErr)
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

func TestNameLockRejectsSymlinkInstalledBeforeOpen(t *testing.T) {
	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, nameLockFilePerm); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), nameLockFilePerm); err != nil {
		t.Fatal(err)
	}
	var hookErr error
	hooks := &nameLockHooks{beforeOpen: func(stage nameLockStage, got string) {
		if stage != stateNameLockStage || got != path || hookErr != nil {
			return
		}
		if err := os.Remove(path); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Symlink(target, path)
	}}
	_, err = lockNameWithHooks(context.Background(), name, hooks)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil ||
		!strings.Contains(err.Error(), "without following links") {
		t.Fatalf("lockName error = %v, want O_NOFOLLOW rejection", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestNameLockRejectsPathReplacementAfterOpen(t *testing.T) {
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
	hooks := &nameLockHooks{afterOpen: func(stage nameLockStage, got string) {
		if stage != stateNameLockStage || got != path || hookErr != nil {
			return
		}
		if err := os.Rename(path, moved); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(path, []byte("replacement"), nameLockFilePerm)
	}}
	_, err = lockNameWithHooks(context.Background(), name, hooks)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "replaced while opening") {
		t.Fatalf("lockName error = %v, want replacement error", err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("opened lock inode was not retained at its old path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "replacement" {
		t.Fatalf("replacement path content = %q", data)
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

	// A recently used file is retained after unlock. It is stale as a
	// lock, but remains the coordination inode and is reusable; the
	// bounded retention sweep only removes old, unlocked files.
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

func TestCleanupNameLockFilesIsBoundedAndSkipsLiveInodes(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * nameLockRetention)
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("%064x.lock", i))
		if err := os.WriteFile(paths[i], nil, nameLockFilePerm); err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			continue
		}
		if err := os.Chtimes(paths[i], old, old); err != nil {
			t.Fatal(err)
		}
	}

	liveUnlock, err := acquireLockFile(context.Background(), stateNameLockStage, paths[1], false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer liveUnlock()
	liveBefore, err := os.Stat(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupNameLockFilesAt(context.Background(), dir, "", time.Now(), 10, 2); err != nil {
		t.Fatalf("cleanupNameLockFilesAt: %v", err)
	}
	for _, index := range []int{0, 2} {
		if _, err := os.Stat(paths[index]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale lock %d remains: %v", index, err)
		}
	}
	liveAfter, err := os.Stat(paths[1])
	if err != nil {
		t.Fatalf("live inode was removed: %v", err)
	}
	if !os.SameFile(liveBefore, liveAfter) {
		t.Fatal("live lock path changed inode during cleanup")
	}
	if _, err := os.Stat(paths[3]); err != nil {
		t.Fatalf("recent lock was removed: %v", err)
	}
}

func TestCleanupNameLockFilesHonorsRetentionCap(t *testing.T) {
	dir := t.TempDir()
	initial := nameLockMaxFiles + 5
	for i := 0; i < initial; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%064x.lock", i))
		if err := os.WriteFile(path, nil, nameLockFilePerm); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupNameLockFiles(context.Background(), dir, "", time.Now()); err != nil {
		t.Fatalf("cleanupNameLockFiles: %v", err)
	}
	remaining := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isNameLockFile(entry.Name()) {
			remaining++
		}
	}
	if remaining >= initial {
		t.Fatalf("retention cap did not remove any files: remaining %d, initial %d", remaining, initial)
	}
	if initial-remaining > nameLockCleanupDelete {
		t.Fatalf("cleanup removed %d files, want at most %d", initial-remaining, nameLockCleanupDelete)
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

func TestNameLockHelperProcess(t *testing.T) {
	if os.Getenv("CONTAINERGO_LOCK_HELPER") != "1" {
		return
	}
	name := os.Getenv("CONTAINERGO_LOCK_NAME")
	mode := os.Getenv("CONTAINERGO_LOCK_MODE")
	if mode == "" {
		mode = "new"
	}
	if mode != "new" {
		var path string
		var err error
		switch mode {
		case "legacy":
			path, err = legacyNameLockPath(name)
		case "cache":
			path, err = transitionalNameLockPath(name)
		default:
			t.Fatalf("unknown helper mode %q", mode)
		}
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		// This intentionally mirrors the old revisions: plain OpenFile
		// followed by flock, without the new path checks.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, nameLockFilePerm)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				fmt.Println("error:", err)
				_ = f.Close()
				return
			}
			time.Sleep(nameLockPoll)
		}
		defer func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}()
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	ctx := context.Background()
	if raw := os.Getenv("CONTAINERGO_LOCK_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	unlock, err := lockName(ctx, name)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer unlock()
	fmt.Println("locked")
}

func TestNameLockSerializesAcrossProcessesWithDifferentTempDirs(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("parent lockName: %v", err)
	}

	runHelper := func(tempDir, timeout string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestNameLockHelperProcess$")
		cmd.Env = append(os.Environ(),
			"CONTAINERGO_LOCK_INHERIT=1",
			"CONTAINERGO_LOCK_HELPER=1",
			"CONTAINERGO_LOCK_NAME="+name,
			"CONTAINERGO_LOCK_TIMEOUT="+timeout,
			"TMPDIR="+tempDir,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lock helper: %v\n%s", err, output)
		}
		return string(output)
	}

	output := runHelper(t.TempDir(), "150ms")
	if !strings.Contains(output, "deadline exceeded") {
		unlock()
		t.Fatalf("held lock helper output = %q, want deadline exceeded", output)
	}
	unlock()

	output = runHelper(t.TempDir(), "1s")
	if !strings.Contains(output, "locked") {
		t.Fatalf("released lock helper output = %q, want locked", output)
	}
}

func startHistoricalLockHelper(t *testing.T, mode, name string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNameLockHelperProcess$")
	cmd.Env = append(os.Environ(),
		"CONTAINERGO_LOCK_INHERIT=1",
		"CONTAINERGO_LOCK_HELPER=1",
		"CONTAINERGO_LOCK_MODE="+mode,
		"CONTAINERGO_LOCK_NAME="+name,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = stdin.Close()
			err := cmd.Wait()
			timedOut := ctx.Err() != nil
			cancel()
			if err != nil && !timedOut {
				t.Errorf("historical lock helper: %v\n%s", err, stderr.String())
			}
		})
	}
	t.Cleanup(stop)

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		stop()
		t.Fatalf("historical helper did not report readiness: %v\n%s", scanner.Err(), stderr.String())
	}
	if scanner.Text() != "locked" {
		stop()
		t.Fatalf("historical helper output = %q\n%s", scanner.Text(), stderr.String())
	}
	return stop
}

func assertHistoricalFlockHeld(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		t.Fatalf("lock %s was not held before the durable barrier", path)
	} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("probe lock %s: %v", path, err)
	}
}

func TestNameLockAcquiresHistoricalBarriersBeforeState(t *testing.T) {
	name := "lock-" + newContainerName()
	statePath, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	stateUnlock, err := acquireLockFile(context.Background(), stateNameLockStage, statePath, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stateUnlock()

	legacyPath, err := legacyNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	cachePath, err := transitionalNameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	reachedState := make(chan struct{})
	var once sync.Once
	hooks := &nameLockHooks{beforeOpen: func(stage nameLockStage, path string) {
		if stage == stateNameLockStage && path == statePath {
			once.Do(func() { close(reachedState) })
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan struct {
		unlock func()
		err    error
	}, 1)
	go func() {
		unlock, err := lockNameWithHooks(ctx, name, hooks)
		result <- struct {
			unlock func()
			err    error
		}{unlock: unlock, err: err}
	}()
	select {
	case <-reachedState:
	case resultValue := <-result:
		if resultValue.unlock != nil {
			resultValue.unlock()
		}
		t.Fatalf("state barrier reached before historical lock setup: %v", resultValue.err)
	case <-ctx.Done():
		t.Fatal("new lock did not reach the durable barrier")
	}
	assertHistoricalFlockHeld(t, legacyPath)
	assertHistoricalFlockHeld(t, cachePath)
	stateUnlock()
	select {
	case resultValue := <-result:
		if resultValue.err != nil {
			t.Fatalf("new lock: %v", resultValue.err)
		}
		resultValue.unlock()
	case <-ctx.Done():
		t.Fatal("new lock did not finish after state release")
	}
}

func TestNameLockSerializesAcrossHistoricalRevisions(t *testing.T) {
	for _, mode := range []string{"legacy", "cache"} {
		t.Run(mode, func(t *testing.T) {
			name := "lock-" + newContainerName()
			durablePath, err := nameLockPath(name)
			if err != nil {
				t.Fatal(err)
			}
			stop := startHistoricalLockHelper(t, mode, name)

			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("new lock with %s holder = %v, want deadline exceeded", mode, err)
			}
			if _, err := os.Stat(durablePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("durable lock opened before historical barrier: %v", err)
			}
			stop()
		})
	}
}
