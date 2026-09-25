//go:build integration

package container_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

// A zero timeout should complete well before the 1.5-second round-up case's
// two-second grace period. The one-second ceiling leaves room for normal CLI
// and scheduler overhead without turning this into a hardware benchmark.
const stopImmediateMaxElapsed = time.Second

// runStopTimingIntegration checks the externally visible stop grace period.
// The process ignores SIGTERM, so a 1.5-second request must not complete at
// the floor-converted one second. The bounds allow normal CLI and scheduler
// overhead; exact equality is not portable across Apple Container and Docker.
func runStopTimingIntegration(t *testing.T, backend string, timeout, minElapsed, maxElapsed time.Duration) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("containergo-stop-%s-%d", backend, os.Getpid())

	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithCmd("sh", "-c", "trap '' TERM; while :; do sleep 1; done"),
		container.WithWaitStrategy(wait.ForExec([]string{"true"}).WithStartupTimeout(30*time.Second)),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state, err := ctr.State(ctx); err != nil {
		t.Fatalf("State before Stop: %v", err)
	} else if state != container.StateRunning {
		t.Fatalf("State before Stop = %q, want %q", state, container.StateRunning)
	}

	start := time.Now()
	if err := ctr.Stop(ctx, &timeout); err != nil {
		t.Fatalf("Stop(%s): %v", timeout, err)
	}
	elapsed := time.Since(start)
	t.Logf("%s Stop(%s) took %s", backend, timeout, elapsed)
	if elapsed < minElapsed {
		t.Errorf("Stop(%s) took %s, want at least %s", timeout, elapsed, minElapsed)
	}
	if elapsed > maxElapsed {
		t.Errorf("Stop(%s) took %s, want at most %s", timeout, elapsed, maxElapsed)
	}
	if state, err := ctr.State(ctx); err != nil {
		t.Fatalf("State after Stop: %v", err)
	} else if state != container.StateStopped {
		t.Errorf("State after Stop = %q, want %q", state, container.StateStopped)
	}
}
