package container

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// The reaper needs /bin/sh, so on Windows registration is a documented no-op
// and cleanup relies on Cleanup and rollback instead. That is a behavioral
// contract, not an accident, so it is asserted rather than only described in
// a comment: if the guard were dropped, registration would silently do nothing
// on Windows while appearing to work everywhere else.
func TestReviewReaperRegistrationIsANoOpOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the no-op contract is specific to Windows")
	}
	// Registering must be safe and must not panic or block on a platform with
	// no shell. The reaper has no process to spawn, so a global registry
	// entry must not be created either.
	before := len(globalReapers)
	registerWithGlobalReaper("docker", "rm", strings.Repeat("a", 64), "")
	if got := len(globalReapers); got != before {
		t.Errorf("globalReapers grew from %d to %d on Windows; registration must be a no-op", before, got)
	}
}

// Cleanup and rollback are the only removal paths on Windows, because the
// reaper is unavailable there. Terminate must therefore work without a reaper.
func TestReviewTerminateWorksWithoutReaper(t *testing.T) {
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
