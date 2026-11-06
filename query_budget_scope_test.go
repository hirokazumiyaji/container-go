package container

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPayloadAndLifecycleOperationsPreserveLongCallerDeadline(t *testing.T) {
	for name, operation := range map[string]struct {
		command string
		call    func(context.Context, *Container) error
	}{
		"copy": {"cp", func(ctx context.Context, c *Container) error {
			return c.CopyContentToContainer(ctx, []byte("payload"), "/tmp/payload", 0o600)
		}},
		"logs": {"logs", func(ctx context.Context, c *Container) error {
			stream, err := c.Logs(ctx)
			if stream != nil {
				_ = stream.Close()
			}
			return err
		}},
		"start":   {"start", func(ctx context.Context, c *Container) error { return c.Start(ctx) }},
		"stop":    {"stop", func(ctx context.Context, c *Container) error { return c.Stop(ctx, nil) }},
		"restart": {"stop", func(ctx context.Context, c *Container) error { return c.Restart(ctx, nil) }},
		"delete":  {"delete", func(ctx context.Context, c *Container) error { return c.Terminate(ctx) }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTestRunner()
			var observed time.Time
			runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
				if args[0] == operation.command {
					observed, _ = ctx.Deadline()
				}
				return f.Run(ctx, args...)
			})
			ctr := runTestContainer(t, runner, WithQueryTimeout(2*time.Second))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			want, _ := ctx.Deadline()
			if err := operation.call(ctx, ctr); err != nil {
				t.Fatal(err)
			}
			if !observed.Equal(want) {
				t.Fatalf("%s deadline=%v, want caller deadline %v", operation.command, observed, want)
			}
		})
	}
}

func TestCreatedOwnershipRetryKeepsOperationBudget(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.queryTimeout = 20 * time.Millisecond
	attempts := 0
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "inspect" {
			attempts++
			if attempts == 1 {
				return nil, nil, errors.New("temporary busy")
			}
		}
		return f.Run(ctx, args...)
	})
	ctr.runner = runner
	cfg := newConfig()
	cfg.eng = appleEngine{}
	cfg.runner = runner
	cfg.queryTimeout = ctr.queryTimeout
	cfg.reusePollInterval = 40 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := verifyCreatedOwnership(ctx, ctr, cfg); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("inspect attempts=%d, want transient attempt followed by success", attempts)
	}
}

func TestTerminateKeepsSuccessfulDeleteAfterCancellation(t *testing.T) {
	f := newTestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deleted := false
	runner := timeoutTestRunner(func(runCtx context.Context, args ...string) ([]byte, []byte, error) {
		stdout, stderr, err := f.Run(runCtx, args...)
		if args[0] == "delete" {
			deleted = true
			cancel()
		}
		return stdout, stderr, err
	})
	ctr := runTestContainer(t, runner, WithLabels(map[string]string{"purpose": "delete-secret"}))
	ctr.pack().dynamic = &engineInfo{state: StateRunning}
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("successful delete returned %v", err)
	}
	if !deleted {
		t.Fatal("delete did not run")
	}
	if ctr.pack().dynamic != nil || ctr.diagnosticRedactorValue != nil {
		t.Fatal("successful termination retained dynamic or diagnostic state")
	}
}

func TestClientDefaultResolutionTimeoutRemainsBestEffort(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "context" {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return nil, nil, nil
	})
	client, err := NewClient(WithClientBackend("docker"), WithClientRunner(runner))
	if err != nil || client == nil {
		t.Fatalf("implicit target resolution timeout: client=%v err=%v, want best-effort client", client, err)
	}
}

func TestStoppedReuseDeletePreservesCallerBudget(t *testing.T) {
	cfg := newConfig()
	cfg.eng = appleEngine{}
	cfg.name = "shared"
	cfg.queryTimeout = 2 * time.Second
	var observed time.Time
	cfg.runner = timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "inspect" {
			return []byte(reuseInspectJSONForCreation("shared", "stopped", "redis:7-alpine", "0123456789abcdef")), nil, nil
		}
		if args[0] == "delete" {
			observed, _ = ctx.Deadline()
		}
		return nil, nil, nil
	})
	info := &engineInfo{state: StateStopped, labels: map[string]string{managedLabel: "true", reuseLabel: "true", creationLabel: "0123456789abcdef"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	deleted, err := deleteStoppedReuseResult(ctx, cfg, info)
	if err != nil || !deleted {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	if !observed.Equal(want) {
		t.Fatalf("delete deadline=%v, want caller deadline=%v", observed, want)
	}
}
