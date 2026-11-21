//go:build !windows

package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// canonicalEnvFileRootForTest returns the root spelling the write path uses.
// t.TempDir may contain a symlinked ancestor on macOS, so comparing against
// the uncanonical path would install the throttle marker under a key no
// write reads.
func canonicalEnvFileRootForTest(t *testing.T) string {
	t.Helper()
	root := isolateEnvFileRoot(t)
	canonical, err := canonicalEnvPath(root)
	if err != nil {
		t.Fatalf("canonicalEnvPath(%q): %v", root, err)
	}
	t.Cleanup(func() { setEnvDirGCLast(t, canonical, time.Time{}) })
	return canonical
}

func setEnvDirGCLast(t *testing.T, root string, at time.Time) {
	t.Helper()
	envDirGCMu.Lock()
	defer envDirGCMu.Unlock()
	if at.IsZero() {
		delete(envDirGCLast, root)
		return
	}
	envDirGCLast[root] = at
}

func writeAndCleanupEnvFile(t *testing.T, base string) {
	t.Helper()
	_, dir, err := writeEnvFileAt(base, map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatalf("writeEnvFileAt: %v", err)
	}
	if err := cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{base: base}); err != nil {
		t.Fatalf("cleanupEnvFileAt: %v", err)
	}
}

func TestWriteEnvFileReclaimsStaleDirsOnlyWhenScanIsDue(t *testing.T) {
	if !envFileLocksSupported {
		t.Skip("advisory env-file locks are unavailable on this platform")
	}
	root := canonicalEnvFileRootForTest(t)
	base := filepath.Dir(root)

	stale, lock := newMarkedStaleEnvDir(t, root, "first")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	writeAndCleanupEnvFile(t, base)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale directory survived the first reclaim scan: %v", err)
	}

	setEnvDirGCLast(t, root, time.Now())
	deferred, lock := newMarkedStaleEnvDir(t, root, "deferred")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	writeAndCleanupEnvFile(t, base)
	if _, err := os.Stat(deferred); err != nil {
		t.Fatalf("stale directory was reclaimed inside the throttle window: %v", err)
	}

	setEnvDirGCLast(t, root, time.Time{})
	writeAndCleanupEnvFile(t, base)
	if _, err := os.Stat(deferred); !os.IsNotExist(err) {
		t.Fatalf("stale directory survived the next due scan: %v", err)
	}
}

func TestWriteEnvFileRetriesUnsafeScanOnNextAttempt(t *testing.T) {
	if !envFileLocksSupported {
		t.Skip("advisory env-file locks are unavailable on this platform")
	}
	root := canonicalEnvFileRootForTest(t)
	base := filepath.Dir(root)

	unsafeDir, lock := newMarkedStaleEnvDir(t, root, "unsafe")
	if err := closeEnvFileLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(unsafeDir, envFileDirMarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeEnvFileAt(base, map[string]string{"TOKEN": "secret"}); err == nil {
		t.Fatal("write succeeded while an unsafe stale entry was due for a scan")
	}
	if !envDirGCDue(root, time.Now()) {
		t.Fatal("failed reclaim scan was recorded as complete")
	}

	if err := os.RemoveAll(unsafeDir); err != nil {
		t.Fatal(err)
	}
	writeAndCleanupEnvFile(t, base)
}
