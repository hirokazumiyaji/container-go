package cli

import (
	"os/exec"
	"runtime"
	"testing"
)

// requirePOSIXShell skips a test that executes a shell stub.
//
// Windows has no /bin/sh, so a stub-based test would fail on the stub rather
// than on the behavior it is about. Not behind a build tag, so the tests that
// call it are still compiled on every platform.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell; not available on Windows")
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("requires /bin/sh: %v", err)
	}
}
