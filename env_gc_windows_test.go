//go:build windows

package container

import (
	"context"
	"os"
	"testing"
	"time"
)

// The Windows write path must throttle its stale-directory scan inside the
// same window as Unix: the scan validates a DACL for every entry, so running
// it on every env write lengthens the root lock critical section. Run's
// explicit cleanup stays immediate.
func TestWindowsEnvDirGCSkipsScanWithinThrottleWindow(t *testing.T) {
	root := t.TempDir()
	envDirGCMu.Lock()
	delete(envDirGCLast, root)
	envDirGCMu.Unlock()
	t.Cleanup(func() {
		envDirGCMu.Lock()
		delete(envDirGCLast, root)
		envDirGCMu.Unlock()
	})

	if err := cleanupWindowsEnvDirsIfDueContext(context.Background(), root); err != nil {
		t.Fatalf("due scan: %v", err)
	}
	if envDirGCDue(root, time.Now()) {
		t.Fatal("a successful scan did not mark the throttle window")
	}

	// Removing the root makes any unthrottled scan fail. The throttled call
	// must succeed without touching the filesystem.
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := cleanupWindowsEnvDirsIfDueContext(context.Background(), root); err != nil {
		t.Fatalf("throttled scan touched the removed root: %v", err)
	}
	if err := cleanupWindowsEnvDirsContext(context.Background(), root); err == nil {
		t.Fatal("explicit cleanup did not scan the removed root")
	}
}
