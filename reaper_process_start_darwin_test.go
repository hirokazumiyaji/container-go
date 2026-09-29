//go:build darwin

package container

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestDarwinProcessStartIdentityIsStableWhileCPUIsBusy(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "while :; do :; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	first, err := reaperProcessStartTime(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatalf("capture initial process start: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	second, err := reaperProcessStartTime(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatalf("recapture busy process start: %v", err)
	}
	if first != second {
		t.Fatalf("busy process identity changed from %q to %q; CPU time cannot identify a process", first, second)
	}
}
