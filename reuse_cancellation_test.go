package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestFollowLogsClassifiesUnsupportedStreamSetup(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())
	_, err := ctr.FollowLogs(context.Background())
	if !errors.Is(err, ErrLogStreamSetup) {
		t.Fatalf("error = %v, want ErrLogStreamSetup", err)
	}
	if !errors.Is(err, wait.ErrLogStreamSetup) {
		t.Fatalf("error = %v, want public wait sentinel identity", err)
	}
}

type streamSetupTarget struct {
	ctr *Container
}

func (*streamSetupTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*streamSetupTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *streamSetupTarget) State(context.Context) (wait.State, error) {
	return wait.StateRunning, nil
}
func (t *streamSetupTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	return t.ctr.FollowLogs(ctx)
}
func (*streamSetupTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

func TestForLogFailsFastForNonExecutableAbsoluteBackend(t *testing.T) {
	backend := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(backend, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctr := &Container{
		id:     "myctr",
		runner: &cli.ExecRunner{Binary: backend},
		eng:    dockerEngine{},
		sync:   &lockPack{uid: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}

	start := time.Now()
	err := wait.ForLog("ready").
		WithStartupTimeout(2*time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), &streamSetupTarget{ctr: ctr})
	if !errors.Is(err, wait.ErrLogStreamSetup) {
		t.Fatalf("error = %v, want ErrLogStreamSetup", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("ForLog took %v, want fail-fast for deterministic setup error", elapsed)
	}
}

func TestReuseInfoRejectsCanceledCachedReadyInfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := &config{}
	ready := &engineInfo{state: StateRunning}

	info, err := reuseInfoForCaller(ctx, cfg, &Container{sync: &lockPack{info: ready}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if info != nil {
		t.Fatalf("info = %+v, want no cached fast-path return", info)
	}
}

func TestReuseWaitRejectsCancellationWithoutStrategy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reuseWait(ctx, &config{}, &Container{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

type cancelOnSuccessStrategy struct {
	cancel context.CancelFunc
}

func (s cancelOnSuccessStrategy) WaitUntilReady(context.Context, wait.Target) error {
	s.cancel()
	return nil
}

func TestReuseWaitRejectsCancellationAfterStrategySuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &config{waitStrategy: cancelOnSuccessStrategy{cancel: cancel}}
	if err := reuseWait(ctx, cfg, &Container{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
