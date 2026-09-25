//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnvFileOwnershipValidation(t *testing.T) {
	root := isolateEnvFileRoot(t)
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	uid := currentEnvFileUID()
	if err := validateEnvOwnership(info, uid, envDirMode); err != nil {
		t.Fatalf("current owner rejected: %v", err)
	}
	if err := validateEnvOwnership(info, uid+1, envDirMode); err == nil {
		t.Fatal("directory with a different owner was accepted")
	}
}

func TestCleanupStaleEnvFilesRequiresMarker(t *testing.T) {
	root := isolateEnvFileRoot(t)
	dir, lock := newMarkedStaleEnvDir(t, root, "missing-marker")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, envFileDirMarkerName)); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err == nil {
		t.Fatal("unmarked directory was eligible for deletion")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("unmarked directory was removed: %v", err)
	}
	if _, _, err := writeEnvFileAt(filepath.Dir(root), map[string]string{"TOKEN": "secret"}); err == nil {
		t.Fatal("unsafe stale entry did not block a new environment file")
	}
}

func TestCleanupStaleEnvFilesRejectsWrongDirectoryMode(t *testing.T) {
	root := isolateEnvFileRoot(t)
	dir, lock := newMarkedStaleEnvDir(t, root, "wrong-mode")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err == nil {
		t.Fatal("directory with non-private mode was eligible for deletion")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("wrong-mode directory was removed: %v", err)
	}
}

func TestCleanupStaleEnvFilesRejectsMarkerSymlink(t *testing.T) {
	root := isolateEnvFileRoot(t)
	dir, lock := newMarkedStaleEnvDir(t, root, "marker-symlink")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside-marker")
	if err := os.WriteFile(target, []byte(envFileDirMarker), envFileMode); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, envFileDirMarkerName)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, marker); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err == nil {
		t.Fatal("directory with a marker symlink was eligible for deletion")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("marker target was removed: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory with marker symlink was removed: %v", err)
	}
}

func TestCleanupStaleEnvFilesRejectsUnexpectedChildren(t *testing.T) {
	root := isolateEnvFileRoot(t)
	dir, lock := newMarkedStaleEnvDir(t, root, "unexpected-child")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-created-by-containergo"), []byte("keep"), envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err == nil {
		t.Fatal("directory with an unexpected child was removed recursively")
	}
	if _, err := os.Stat(filepath.Join(dir, "not-created-by-containergo")); err != nil {
		t.Fatalf("unexpected child was removed: %v", err)
	}
}

func TestEnvPathCanonicalizesTrustedSymlinkAncestor(t *testing.T) {
	realParent := t.TempDir()
	realParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatal(err)
	}
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "cache-link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	base := filepath.Join(link, "cache")
	path, dir, err := writeEnvFileAt(base, map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatalf("write through trusted symlink ancestor: %v", err)
	}
	if !strings.HasPrefix(path, filepath.Join(realParent, "cache")+string(filepath.Separator)) {
		t.Fatalf("returned path %q was not canonicalized under %q", path, realParent)
	}
	if err := cleanupEnvFileAt(filepath.Dir(filepath.Dir(dir)), dir, removeExpectedEnvChildren); err != nil {
		t.Fatal(err)
	}
}

func TestEnvPathRejectsParentTraversalAndWritableAncestor(t *testing.T) {
	base := t.TempDir()
	traversal := base + string(filepath.Separator) + "child" + string(filepath.Separator) + ".."
	if _, _, err := writeEnvFileAt(traversal, map[string]string{"TOKEN": "secret"}); !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("parent-traversal path error = %v, want unsafe storage error", err)
	}
	writable := filepath.Join(base, "writable")
	if err := os.Mkdir(writable, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeEnvFileAt(filepath.Join(writable, "cache"), map[string]string{"TOKEN": "secret"}); !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("writable-ancestor path error = %v, want unsafe storage error", err)
	}
}

func TestCleanupStaleEnvFilesRootSymlinkFailsClosed(t *testing.T) {
	base := t.TempDir()
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "keep")
	if err := os.WriteFile(victimFile, []byte("keep"), envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(base, envFileRootName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, _, err := writeEnvFileAt(base, map[string]string{"TOKEN": "secret"}); !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("writeEnvFileAt error = %v, want unsafe storage error", err)
	}
	if data, err := os.ReadFile(victimFile); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

type reuseEnvCleanupFailureRunner struct {
	*reuseCreateRunner
	envPath string
}

func (r *reuseEnvCleanupFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		for i, arg := range args {
			if arg == "--env-file" && i+1 < len(args) {
				r.envPath = args[i+1]
				// Simulate a process losing the marker just before the
				// backend returns. The create itself succeeds, but the
				// first cleanup must not turn the successful reuse result
				// into an unowned container.
				_ = os.Remove(filepath.Join(filepath.Dir(r.envPath), envFileDirMarkerName))
			}
		}
	}
	return r.reuseCreateRunner.Run(ctx, args...)
}

func TestPublicReuseReturnsHandleWhenEnvCleanupFails(t *testing.T) {
	isolateEnvFileRoot(t)
	runner := &reuseEnvCleanupFailureRunner{reuseCreateRunner: newReuseCreateRunner()}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("env-reuse"), WithReuse(), WithEnv(map[string]string{"TOKEN": "secret"}),
		withRunner(runner), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("expected the injected env cleanup error")
	}
	if ctr == nil || ctr.ID() != "env-reuse" {
		t.Fatalf("Run returned handle %v, error %v; want usable reuse handle", ctr, err)
	}
	if !errors.Is(err, errUnsafeEnvFile) && !strings.Contains(err.Error(), envFileDirMarkerName) {
		t.Fatalf("cleanup error = %v, want marker safety error", err)
	}
	if runner.envPath == "" {
		t.Fatal("runner did not observe env-file path")
	}
	dir := filepath.Dir(runner.envPath)
	if state := loadEnvCleanupState(dir); state != nil {
		state.mu.Lock()
		lock := state.lock
		state.lock = nil
		state.mu.Unlock()
		if lock != nil {
			_ = closeEnvFileLock(lock)
		}
		clearEnvCleanupState(state)
	}
	_ = os.RemoveAll(dir)
}

func TestRunRejectsReplacedEnvRootBeforeBackend(t *testing.T) {
	root := isolateEnvFileRoot(t)
	realRoot := root + "-real"
	if err := os.Rename(root, realRoot); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "keep")
	if err := os.WriteFile(victimFile, []byte("keep"), envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithEnv(map[string]string{"TOKEN": "secret"}),
		withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("Run error = %v, want unsafe storage error", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Run invoked the backend with an unsafe environment root: %v", f.calls)
	}
	if data, err := os.ReadFile(victimFile); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

func TestCleanupStaleEnvFilesStagesByAgeAndChildren(t *testing.T) {
	root := isolateEnvFileRoot(t)
	old := filepath.Join(root, envFileStagingPrefix+"old")
	if err := os.Mkdir(old, envDirMode); err != nil {
		t.Fatal(err)
	}
	recent := filepath.Join(root, envFileStagingPrefix+"recent")
	if err := os.Mkdir(recent, envDirMode); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * envFileStagingStaleAfter)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old empty staging directory remains: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent staging directory was removed: %v", err)
	}

	unexpected := filepath.Join(root, envFileStagingPrefix+"unexpected")
	if err := os.Mkdir(unexpected, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unexpected, "unexpected"), []byte("keep"), envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unexpected, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err == nil {
		t.Fatal("old unmarked staging directory with a child was removed")
	}
	if _, err := os.Stat(filepath.Join(unexpected, "unexpected")); err != nil {
		t.Fatalf("old staging child was removed: %v", err)
	}
}

func TestRootMarkerCreationIsRaceSafe(t *testing.T) {
	base := t.TempDir()
	const workers = 32
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- withEnvFileRoot(base, func(string) error { return nil })
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent root initialization: %v", err)
		}
	}
	marker, err := os.ReadFile(filepath.Join(base, envFileRootName, envFileRootMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != envFileRootLockMarker {
		t.Fatalf("root marker = %q, want %q", marker, envFileRootLockMarker)
	}
}

func TestRootMarkerCrashIsRecoverableOnlyWhenRootIsEmpty(t *testing.T) {
	base := t.TempDir()
	if err := withEnvFileRoot(base, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, envFileRootName)
	markerPath := filepath.Join(root, envFileRootMarkerName)
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	partial := envFileRootLockMarker[:len(envFileRootLockMarker)/2]
	if err := os.WriteFile(markerPath, []byte(partial), envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(markerPath, envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := withEnvFileRoot(base, func(string) error { return nil }); err != nil {
		t.Fatalf("recover root marker: %v", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != envFileRootLockMarker {
		t.Fatalf("recovered marker = %q, want %q", marker, envFileRootLockMarker)
	}

	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, envFileDirPrefix+"manual")
	if err := os.Mkdir(child, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := withEnvFileRoot(base, func(string) error { return nil }); !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("root with child and missing marker error = %v, want unsafe storage error", err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("manual child was removed: %v", err)
	}
}

func TestCleanupStaleEnvFilesRepairsMarkerBeforeLock(t *testing.T) {
	root := isolateEnvFileRoot(t)
	staging := filepath.Join(root, envFileStagingPrefix+"marker-before-lock")
	if err := os.Mkdir(staging, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staging, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvMarker(filepath.Join(staging, envFileDirMarkerName), envFileDirMarker); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * envFileStagingStaleAfter)
	if err := os.Chtimes(staging, old, old); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err != nil {
		t.Fatalf("cleanup marker-before-lock staging: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("marker-before-lock staging directory remains: %v", err)
	}
}

func TestCleanupStaleEnvFilesRepairsPartialStagingMarker(t *testing.T) {
	root := isolateEnvFileRoot(t)
	staging := filepath.Join(root, envFileStagingPrefix+"partial-marker")
	if err := os.Mkdir(staging, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staging, envDirMode); err != nil {
		t.Fatal(err)
	}
	partial := envFileDirMarker[:len(envFileDirMarker)/2]
	if err := os.WriteFile(filepath.Join(staging, envFileDirMarkerName), []byte(partial), envFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(staging, envFileDirMarkerName), envFileMode); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * envFileStagingStaleAfter)
	if err := os.Chtimes(staging, old, old); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err != nil {
		t.Fatalf("cleanup partial staging marker: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("partial staging directory remains: %v", err)
	}
}

func TestCleanupStaleEnvFilesRepairsTombstoneAfterPartialRemoval(t *testing.T) {
	root := isolateEnvFileRoot(t)
	dir, lock := newMarkedStaleEnvDir(t, root, "tombstone-partial")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	tombstone := filepath.Join(root, envFileTombstonePrefix+"tombstone-partial")
	if err := os.Rename(dir, tombstone); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tombstone, envFileName)); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err != nil {
		t.Fatalf("cleanup partial tombstone: %v", err)
	}
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("partial tombstone remains: %v", err)
	}
}

func TestConcurrentEnvCleanupRetainsSingleOwner(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- cleanupEnvFile(dir)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent cleanup: %v", err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains: %v", err)
	}
	if state := loadEnvCleanupState(dir); state != nil {
		t.Fatalf("cleanup ownership remains after all retries: %#v", state)
	}
}

func TestCleanupStaleEnvFilesConcurrentWithLiveWriter(t *testing.T) {
	root := isolateEnvFileRoot(t)
	dir, lock := newMarkedStaleEnvDir(t, root, "concurrent")
	if err := acquireEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- cleanupStaleEnvFilesAt(filepath.Dir(root))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent stale cleanup: %v", err)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live writer directory was removed: %v", err)
	}
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleEnvFilesAt(filepath.Dir(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("crashed writer directory remains: %v", err)
	}
}

func newMarkedStaleEnvDir(t *testing.T, root, suffix string) (string, *os.File) {
	t.Helper()
	dir := filepath.Join(root, envFileDirPrefix+suffix)
	if err := os.Mkdir(dir, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker); err != nil {
		t.Fatal(err)
	}
	lock, err := openEnvFileNoFollow(filepath.Join(dir, envFileLockName), os.O_CREATE|os.O_RDWR, envFileMode)
	if err != nil {
		t.Fatal(err)
	}
	envFile, err := openEnvFileNoFollow(filepath.Join(dir, envFileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, envFileMode)
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if _, err := envFile.WriteString("TOKEN=secret\n"); err != nil {
		_ = envFile.Close()
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := envFile.Close(); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	return dir, lock
}
