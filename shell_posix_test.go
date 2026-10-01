package container

import (
	"os/exec"
	"runtime"
	"testing"
)

// requirePOSIXShell skips a test that depends on a POSIX shell stub or a
// /bin/sh child.
//
// Windows has no /bin/sh, so such a test cannot exercise what it is about: it
// would fail on the stub rather than on the behavior. This helper is
// deliberately not behind a build tag, so the tests that call it are still
// compiled on every platform and a Windows compile regression is caught. The
// check is on runtime rather than GOOS for the same reason: `GOOS=windows go
// vet` should still type-check these files.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell; not available on Windows")
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("requires /bin/sh: %v", err)
	}
}
