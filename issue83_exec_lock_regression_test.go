//go:build !windows

package container

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
)

type blockingAppleExecRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingAppleExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
			return []byte("ok\n"), nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *blockingAppleExecRunner) unblock() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

type execForAnyStrategy struct{}

func (execForAnyStrategy) WaitUntilReady(ctx context.Context, target wait.Target) error {
	_, err := target.ExecCommand(ctx, []string{"long-running-command"})
	return err
}

type runningForAnyStrategy struct {
	started <-chan struct{}
}

func (s runningForAnyStrategy) WaitUntilReady(ctx context.Context, target wait.Target) error {
	select {
	case <-s.started:
	case <-ctx.Done():
		return ctx.Err()
	}
	running, err := target.Running(ctx)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("container is not running")
	}
	return nil
}

func TestForAnyCanProbeWhileAppleExecIsRunning(t *testing.T) {
	runner := &blockingAppleExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, runner)
	t.Cleanup(runner.unblock)

	err := wait.ForAny(execForAnyStrategy{}, runningForAnyStrategy{started: runner.started}).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err != nil {
		t.Fatalf("ForAny with a blocked Exec: %v", err)
	}
	runner.unblock()
}

func TestAppleExecReleasesNameLockBeforeRunner(t *testing.T) {
	runner := &blockingAppleExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, runner)
	t.Cleanup(runner.unblock)

	execDone := make(chan error, 1)
	go func() {
		_, _, err := ctr.Exec(context.Background(), []string{"long-running-command"})
		execDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("Exec did not reach the runner")
	}

	stateDone := make(chan error, 1)
	go func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := ctr.State(probeCtx)
		stateDone <- err
	}()
	select {
	case err := <-stateDone:
		if err != nil {
			t.Fatalf("State while Apple Exec is blocked: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("State deadlocked behind the Apple Exec name lock")
	}

	runner.unblock()
	select {
	case err := <-execDone:
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Exec did not finish after releasing the runner")
	}
}
