//go:build linux

package cli

import (
	"os/exec"
	"testing"
)

func TestLinuxProcessIdentityDoesNotClaimExitedChild(t *testing.T) {
	cmd := exec.Command(writeStub(t, `exit 0`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitForProcessExitNoReap(cmd) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skip("could not observe the child exit without reaping it")
	}
	tree, err := newProcessTree(cmd)
	if err != nil {
		_ = cmd.Wait()
		t.Skipf("pidfd is unavailable on this host: %v", err)
	}
	result := tree.terminate(cmd)
	tree.close()
	if result.active {
		t.Fatalf("termination result = %+v, exited child must not be active", result)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait after pidfd termination probe: %v", err)
	}
}

func TestLinuxProcessIdentityDoesNotUseRecycledPIDAfterReap(t *testing.T) {
	cmd := exec.Command(writeStub(t, `exit 0`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree, err := newProcessTree(cmd)
	if err != nil {
		_ = cmd.Wait()
		t.Skipf("pidfd is unavailable on this host: %v", err)
	}
	if !waitForProcessExitNoReap(cmd) {
		tree.close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skip("could not observe the child exit without reaping it")
	}
	if err := cmd.Wait(); err != nil {
		tree.close()
		t.Fatalf("Wait: %v", err)
	}

	// The numeric PID may now be recycled. The retained pidfd still names
	// the original child, so termination must not signal its former group.
	result := tree.terminate(cmd)
	tree.close()
	if result.active {
		t.Fatalf("termination result = %+v, reaped child must not be active", result)
	}
}
