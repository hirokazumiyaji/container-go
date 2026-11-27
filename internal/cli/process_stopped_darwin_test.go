//go:build darwin

package cli

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

// TestDarwinObservesOnlyTheRequestedStoppedChild pins waitid's idtype to
// P_PID. With P_ALL, waitid reports any stopped child in this process, so a
// concurrent CLI child that accepted SIGSTOP would make termination believe
// an unrelated running child had stopped and release the process-group kill
// through the stopped-state barrier.
func TestDarwinObservesOnlyTheRequestedStoppedChild(t *testing.T) {
	running := exec.CommandContext(context.Background(), writeStub(t, `exec sleep 30`))
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = running.Process.Kill()
		_ = running.Wait()
	})

	stopped := exec.CommandContext(context.Background(), writeStub(t, `exec sleep 30`))
	if err := stopped.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stopped.Process.Kill()
		_ = stopped.Wait()
	})
	if err := stopped.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	observed, err := observeProcessStopped(stopped.Process)
	if err != nil {
		t.Fatalf("observe stopped child: %v", err)
	}
	if !observed {
		t.Fatal("stopped child was not observed; the stop barrier cannot be trusted")
	}

	observed, err = observeProcessStopped(running.Process)
	if err != nil {
		t.Fatalf("observe running child: %v", err)
	}
	if observed {
		t.Fatal("a concurrently stopped child satisfied the observation for a running process")
	}
}
