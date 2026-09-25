//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
