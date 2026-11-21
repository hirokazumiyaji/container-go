//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletedEnvCleanupStateIsBounded(t *testing.T) {
	oldLimit := completedEnvCleanupLimit
	completedEnvCleanupLimit = 3
	reset := func() {
		completedEnvCleanupMu.Lock()
		completedEnvCleanups.Range(func(key, _ any) bool {
			completedEnvCleanups.Delete(key)
			return true
		})
		completedEnvCleanupOrder = nil
		completedEnvCleanupKeys = nil
		completedEnvCleanupMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		reset()
		completedEnvCleanupLimit = oldLimit
	})

	root := t.TempDir()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, "one"),
		filepath.Join(root, "two"),
		filepath.Join(root, "three"),
		filepath.Join(root, "four"),
		filepath.Join(root, "five"),
	}
	for _, path := range paths {
		recordCompletedEnvCleanup(&activeEnvFile{
			root:     root,
			rootInfo: info,
			path:     path,
			original: path,
		})
	}
	if _, ok := loadCompletedEnvCleanup(paths[0]); ok {
		t.Fatal("oldest completed cleanup was not evicted")
	}
	if _, ok := loadCompletedEnvCleanup(paths[len(paths)-1]); !ok {
		t.Fatal("newest completed cleanup was not retained")
	}
}

func TestWriteEnvFileUsesPrivatePermissions(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret=value"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupEnvFile(dir); err != nil {
			t.Errorf("cleanupEnvFile: %v", err)
		}
	})

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat env directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != envDirMode {
		t.Errorf("env directory mode = %04o, want %04o", got, envDirMode)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != envFileMode {
		t.Errorf("env file mode = %04o, want %04o", got, envFileMode)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if got, want := string(data), "TOKEN=secret=value\n"; got != want {
		t.Errorf("env file = %q, want %q", got, want)
	}

	if err := cleanupEnvFile(dir); err != nil {
		t.Fatalf("cleanupEnvFile: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("env directory remains after cleanup: %v", err)
	}
}

func TestWriteEnvFileCleansUpAfterLateRootCloseError(t *testing.T) {
	if !envFileLocksSupported {
		t.Skip("secure environment files are unsupported on this platform")
	}
	base := t.TempDir()
	lateErr := errors.New("injected root lock close failure")
	path, dir, err := writeEnvFileAtWithRoot(base, map[string]string{"TOKEN": "secret"}, func(base string, fn func(string) error) error {
		return withEnvFileRootWithClose(base, fn, func(marker *os.File) error {
			_ = closeEnvFileLock(marker)
			return lateErr
		})
	})
	if !errors.Is(err, lateErr) {
		t.Fatalf("write error = %v, want late root close error", err)
	}
	if path != "" || dir != "" {
		t.Fatalf("write returned live path after cleanup: path=%q dir=%q", path, dir)
	}
	root := filepath.Join(base, envFileRootName)
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), envFileDirPrefix) ||
			strings.HasPrefix(entry.Name(), envFileStagingPrefix) ||
			strings.HasPrefix(entry.Name(), envFileTombstonePrefix) {
			t.Fatalf("environment artifact %q remained after root close failure", entry.Name())
		}
	}
}

func TestCleanupRetainsOwnershipAfterLateCloseError(t *testing.T) {
	root := isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	lateErr := errors.New("injected late lock close failure")
	err = cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{
		base: filepath.Dir(root),
		closeLock: func(lock *os.File) error {
			_ = closeEnvFileLock(lock)
			return lateErr
		},
	})
	if !errors.Is(err, lateErr) {
		t.Fatalf("cleanup error = %v, want late close error", err)
	}
	state := loadEnvCleanupState(dir)
	if state == nil || !state.pending {
		t.Fatalf("cleanup ownership was lost after late close error: %#v", state)
	}
	if err := cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{base: filepath.Dir(root)}); err != nil {
		t.Fatalf("cleanup retry after late close error: %v", err)
	}
	if state := loadEnvCleanupState(dir); state != nil {
		t.Fatalf("cleanup state remains after successful retry: %#v", state)
	}
}

func TestCleanupEnvFileReturnsRemovalFailureAndCanRetry(t *testing.T) {
	root := isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	removeFailure := errors.New("injected removal failure")
	attempts := 0
	err = cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{
		base: filepath.Dir(root),
		removeAll: func(path string) error {
			attempts++
			if attempts == 1 {
				if err := os.Remove(filepath.Join(path, envFileDirMarkerName)); err != nil {
					return err
				}
				return removeFailure
			}
			return os.RemoveAll(path)
		},
	})
	if !errors.Is(err, removeFailure) {
		t.Fatalf("cleanup error = %v, want injected removal failure", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("original environment path was not handed off to a tombstone: %v", err)
	}
	state := loadEnvCleanupState(dir)
	if state == nil || !state.pending || state.path == "" {
		t.Fatalf("cleanup ownership was not retained for retry: %#v", state)
	}
	if _, err := os.Stat(state.path); err != nil {
		t.Fatalf("cleanup tombstone is missing after failed cleanup: %v", err)
	}

	// cleanupEnvFileAt models the deferred retry used by Run, reuse, and
	// Exec. A partial first RemoveAll is allowed to finish on the retry.
	if err := cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{base: filepath.Dir(root), removeAll: os.RemoveAll}); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains after retry: %v", err)
	}
}

func TestCleanupStaleEnvFilesRespectsWriterLock(t *testing.T) {
	if !envFileLocksSupported {
		t.Skip("advisory env-file locks are unavailable on this platform")
	}
	root := isolateEnvFileRoot(t)
	dir := filepath.Join(root, envFileDirPrefix+"crashed")
	if err := os.Mkdir(dir, envDirMode); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker); err != nil {
		t.Fatal(err)
	}
	lock, err := openEnvFileNoFollow(filepath.Join(dir, envFileLockName), os.O_CREATE|os.O_RDWR, envFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := acquireEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, envFileName), []byte("SECRET"), envFileMode); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleEnvFiles(); err != nil {
		t.Fatalf("cleanupStaleEnvFiles: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live env directory was removed: %v", err)
	}
	if err := releaseEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()

	if err := cleanupStaleEnvFiles(); err != nil {
		t.Fatalf("cleanupStaleEnvFiles: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("unlocked env directory remains: %v", err)
	}
}
