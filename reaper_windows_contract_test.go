package container

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The native reaper replaced the old Windows no-op contract: Windows has no
// /bin/sh, so registration there must create a real watchdog and the
// self-executed child must delete a registered immutable Docker ID after EOF.
// This is a behavioral contract, not an accident, so it is asserted.
func TestReaperRegistrationIsNativeOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Windows reaper contract is specific to Windows")
	}
	if !nativeReaperSupported() {
		t.Fatal("the Windows test binary must be able to self-execute the reaper")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	uid := nativeReaperID('a')
	globalReapersMu.Lock()
	delete(globalReapers, exe)
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		r := globalReapers[exe]
		delete(globalReapers, exe)
		globalReapersMu.Unlock()
		if r != nil {
			r.closeStdin()
		}
	})

	if err := registerWithGlobalReaper(exe, "rm", nil, uid, ""); err != nil {
		t.Fatalf("registerWithGlobalReaper: %v", err)
	}
	globalReapersMu.Lock()
	watchdog := globalReapers[exe]
	globalReapersMu.Unlock()
	if watchdog == nil {
		t.Fatal("registration was a no-op on Windows")
	}
	watchdog.closeStdin()
	waitForNativeReaperLog(t, logPath, "--force "+uid)
	waitForNativeReaperExit(t, watchdog)

	if data, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(data), "--force "+uid) {
		t.Fatalf("backend log = %q (%v), want the registered Docker ID reaped", data, err)
	}
}

// Explicit removal paths remain available on every platform, because
// cleanup must not depend on the watchdog having run.
func TestTerminateWorksWithoutReaper(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Errorf("Terminate: %v", err)
	}
	// The Apple backend's delete verb is `delete`; the reaper's is `rm`.
	if call := f.callWith("delete"); call == nil {
		t.Errorf("Terminate did not delete the container; calls: %v", f.calls)
	}
}
