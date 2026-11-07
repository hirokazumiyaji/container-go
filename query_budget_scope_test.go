package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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

func TestClientResolutionPreservesLongCallerDeadline(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "long-context")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	var observed time.Time
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "context" {
			observed, _ = ctx.Deadline()
			return []byte(`[{"Name":"long-context","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil, nil
		}
		return nil, nil, nil
	})
	if _, err := NewClientContext(ctx, WithClientBackend("docker"), WithClientRunner(runner)); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(want) {
		t.Fatalf("resolution deadline=%v, want caller deadline=%v", observed, want)
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

func TestPullResolutionPreservesLongCallerDeadline(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	var observed time.Time
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "context" {
			observed, _ = ctx.Deadline()
			return []byte(`[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil, nil
		}
		return nil, nil, nil
	})
	if err := pullWith(ctx, runner, dockerEngine{}, "redis:7-alpine"); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(want) {
		t.Fatalf("resolution deadline=%v, want caller deadline=%v", observed, want)
	}
}

func TestReusePullRunDeadlineIsTerminal(t *testing.T) {
	for name, policy := range map[string]PullPolicy{"missing": PullMissing, "always": PullAlways} {
		t.Run(name, func(t *testing.T) {
			f := newTestRunner()
			pulls := 0
			runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
				if args[0] == "inspect" {
					return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: `Error: notFound: "container not found: myctr"`}
				}
				if len(args) > 1 && args[0] == "image" && args[1] == "pull" {
					pulls++
					<-ctx.Done()
					return nil, nil, ctx.Err()
				}
				return f.Run(ctx, args...)
			})
			cfg := newConfig()
			cfg.eng = appleEngine{}
			cfg.runner = runner
			cfg.name = "myctr"
			cfg.pullPolicy = policy
			cfg.runTimeout = 10 * time.Millisecond
			cfg.reusePollInterval = time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := reuseEnsureContainer(ctx, "redis:7-alpine", cfg)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v, want run deadline", err)
			}
			if pulls != 1 {
				t.Fatalf("pulls=%d, want one terminal fetch", pulls)
			}
		})
	}
}
