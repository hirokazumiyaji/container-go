package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestReview91FollowLogsClassifiesUnsupportedStreamSetup(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())
	_, err := ctr.FollowLogs(context.Background())
	if !errors.Is(err, ErrLogStreamSetup) {
		t.Fatalf("error = %v, want ErrLogStreamSetup", err)
	}
	if !errors.Is(err, wait.ErrLogStreamSetup) {
		t.Fatalf("error = %v, want public wait sentinel identity", err)
	}
}

func TestReview91ForLogFailsFastForNonExecutableAbsoluteBackend(t *testing.T) {
	backend := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(backend, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctr := &Container{
		id:     "myctr",
		runner: &cli.ExecRunner{Binary: backend},
		eng:    dockerEngine{},
	}

	start := time.Now()
	err := wait.ForLog("ready").
		WithStartupTimeout(2*time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if !errors.Is(err, wait.ErrLogStreamSetup) {
		t.Fatalf("error = %v, want ErrLogStreamSetup", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("ForLog took %v, want fail-fast for deterministic setup error", elapsed)
	}
}

func TestReview91ReuseInfoRejectsCanceledCachedReadyInfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := &config{}
	ready := &engineInfo{state: StateRunning}

	info, err := reuseInfoForCaller(ctx, cfg, ready)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if info != nil {
		t.Fatalf("info = %+v, want no cached fast-path return", info)
	}
}

func TestReview91ReuseWaitRejectsCancellationWithoutStrategy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reuseWait(ctx, &config{}, &Container{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

type review91CancelOnSuccessStrategy struct {
	cancel context.CancelFunc
}

func (s review91CancelOnSuccessStrategy) WaitUntilReady(context.Context, wait.Target) error {
	s.cancel()
	return nil
}

func TestReview91ReuseWaitRejectsCancellationAfterStrategySuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &config{waitStrategy: review91CancelOnSuccessStrategy{cancel: cancel}}
	if err := reuseWait(ctx, cfg, &Container{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
