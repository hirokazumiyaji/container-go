//go:build linux

package cli

import (
	"errors"
	"os/exec"
	"syscall"
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

type ownershipLossIdentity struct {
	killCalls int
}

func (*ownershipLossIdentity) active() (bool, error) { return true, nil }
func (*ownershipLossIdentity) stop() error           { return nil }
func (*ownershipLossIdentity) stopped() (bool, error) {
	return false, nil
}
func (p *ownershipLossIdentity) kill() error {
	p.killCalls++
	return nil
}
func (*ownershipLossIdentity) groupID() (int, bool) {
	panic("numeric PGID signaling after waitid ownership loss")
}
func (*ownershipLossIdentity) close() {}

// This models cmd.Wait winning the reap race after SIGSTOP: waitid no longer
// exposes a stopped state, so termination must use only the pidfd reference.
func TestLinuxTerminationInterleavingWithWaitReapUsesPidfd(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	identity := &ownershipLossIdentity{}
	result := (&unixProcessTree{identity: identity}).terminate(cmd)
	if result.active {
		t.Fatalf("termination result = %+v, must not claim the child is still active", result)
	}
	if result.err != nil && !errors.Is(result.err, errProcessStopNotObserved) {
		t.Fatalf("termination result = %+v, want pidfd kill with optional stop-observation error", result)
	}
	if identity.killCalls != 1 {
		t.Fatalf("pidfd kill calls = %d, want 1", identity.killCalls)
	}
}

// reapedKillIdentity reports a real child's state but fails its direct kill
// with ESRCH, which is what the pidfd path returns once the numeric group
// signal has killed the child and the sole waiter has already reaped it.
type reapedKillIdentity struct {
	stableProcessIdentity
}

func (reapedKillIdentity) kill() error { return syscall.ESRCH }

// TestLinuxTerminationAcceptsReapedChildAfterGroupKill pins the race that made
// TestExecRunnerStatusTracksContextTermination flake. The group signal kills
// the child, cmd.Wait reaps it, and the then-redundant pidfd SIGKILL reports
// ESRCH. That is a proven termination, so it must not be reported as a failure
// to prove one.
func TestLinuxTerminationAcceptsReapedChildAfterGroupKill(t *testing.T) {
	cmd := exec.Command(writeStub(t, `sleep 5`))
	configureProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	identity, err := openProcessIdentity(cmd.Process)
	if err != nil {
		t.Skipf("pidfd is unavailable on this host: %v", err)
	}
	defer identity.close()

	result := terminateProcessIdentity(reapedKillIdentity{identity}, cmd.Process.Pid)
	if !result.active || result.err != nil {
		t.Fatalf("termination result = %+v, want proven active termination", result)
	}
}
