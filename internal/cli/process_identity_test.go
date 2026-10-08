//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

package cli

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestRetainedUnixProcessTreeAfterWaitUsesReleasedHandle(t *testing.T) {
	cmd := exec.Command(writeStub(t, "exit 0"))
	configureProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree, err := newProcessTree(cmd)
	if err != nil {
		_ = cmd.Wait()
		t.Fatalf("newProcessTree: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		tree.close()
		t.Fatalf("Wait: %v", err)
	}
	result := tree.terminate(cmd)
	tree.close()
	if result.active || !errors.Is(result.err, os.ErrProcessDone) {
		t.Fatalf("post-Wait termination = %+v, want released direct handle", result)
	}
}

type freshBarrierIdentity struct {
	killCalls int
}

func (*freshBarrierIdentity) active() (bool, error) { return true, nil }
func (*freshBarrierIdentity) stop() error           { return nil }
func (*freshBarrierIdentity) stopped() (bool, error) {
	return false, nil
}
func (p *freshBarrierIdentity) kill() error {
	p.killCalls++
	return nil
}
func (*freshBarrierIdentity) groupID() (int, bool) {
	panic("numeric group signal attempted without stopped barrier")
}
func (*freshBarrierIdentity) close() {}

func TestUnixTerminationFallsBackWhenStopIsNotObserved(t *testing.T) {
	identity := &freshBarrierIdentity{}
	result := terminateProcessIdentity(identity, 12345)
	if result.active || result.err == nil {
		t.Fatalf("termination result = %+v, want conservative identity-safe direct fallback", result)
	}
	if identity.killCalls != 1 {
		t.Fatalf("direct identity kills = %d, want 1", identity.killCalls)
	}
}
