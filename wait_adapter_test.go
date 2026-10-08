package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

type waitRunnerFunc func(context.Context, ...string) ([]byte, []byte, error)

func (f waitRunnerFunc) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return f(ctx, args...)
}

func TestWaitTargetBackendStatePolicy(t *testing.T) {
	cases := []struct {
		name   string
		engine engine
		status string
		want   wait.State
	}{
		{name: "Apple created", engine: appleEngine{}, status: "created", want: wait.StateCreated},
		{name: "Apple stopping is terminal", engine: appleEngine{}, status: "stopping", want: wait.StateStopping},
		{name: "Apple unknown", engine: appleEngine{}, status: "future-state", want: wait.StateUnknown},
		{name: "Docker restarting is transient", engine: dockerEngine{}, status: "restarting", want: wait.StateRestarting},
		{name: "Docker paused is terminal", engine: dockerEngine{}, status: "paused", want: wait.StatePaused},
		{name: "Docker unknown", engine: dockerEngine{}, status: "future-state", want: wait.StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validGen := "0123456789abcdef"
			validUID := strings.Repeat("a", 64)
			runner := waitRunnerFunc(func(_ context.Context, _ ...string) ([]byte, []byte, error) {
				if tc.engine.name() == "apple" {
					return []byte(fmt.Sprintf(`[{"id":"myctr","configuration":{"labels":{%q:"true",%q:%q,%q:%q}},"status":{"state":%q}}]`,
						managedLabel, sessionLabel, sessionID(), creationLabel, validGen, tc.status)), nil, nil
				}
				return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":%q}}]`, validUID, tc.status)), nil, nil
			})
			ctr := &Container{id: "myctr", runner: runner, eng: tc.engine}
			if tc.engine.name() == "apple" {
				ctr.creation = validGen
			} else {
				ctr.uid = validUID
			}
			target := waitTarget{c: ctr}

			got, err := target.State(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tc.want {
				t.Errorf("State = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitTargetReportsUndeclaredDefaultPort(t *testing.T) {
	target := waitTarget{c: &Container{}}
	_, err := target.Endpoint(context.Background(), "")
	if err == nil {
		t.Fatal("want undeclared-port error")
	}
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed", err)
	}
}

func TestWaitUndeclaredPortErrorRemainsMatchable(t *testing.T) {
	target := waitTarget{c: &Container{}}
	err := wait.ForListeningPort("6379/tcp").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed", err)
	}
}

func TestWaitImplicitPortSelectsOnlyTCP(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("53/udp", "80/tcp"))
	target := waitTarget{c: ctr}
	endpoint, err := target.Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "192.168.64.3:80" {
		t.Fatalf("endpoint = %q, want first TCP port", endpoint)
	}
}

func TestWaitImplicitPortPreservesExposedDeclarationOrder(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f,
		WithExposedPorts("80/tcp"),
		WithPublishedPort("127.0.0.1:18081:8081/tcp"),
	)
	endpoint, err := (waitTarget{c: ctr}).Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "192.168.64.3:80" {
		t.Fatalf("endpoint = %q, want first declared exposed TCP port", endpoint)
	}
}

func TestWaitImplicitPortFallsBackToPublishedTCP(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithPublishedPort("127.0.0.1:18081:8081/tcp"))
	endpoint, err := (waitTarget{c: ctr}).Endpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "127.0.0.1:18081" {
		t.Fatalf("endpoint = %q, want published-only TCP port", endpoint)
	}
}

func TestWaitImplicitPortRejectsUDPOnlyContainer(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("53/udp"))
	_, err := (waitTarget{c: ctr}).Endpoint(context.Background(), "")
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed for UDP-only container", err)
	}
}

func TestRunRejectsUndeclaredWaitPortBeforeImageLookup(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever),
		WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
		withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("Run error = %v, want ErrPortNotExposed", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called before port validation: %v", f.calls)
	}
}

func TestRunValidatesWaitBeforeMissingImage(t *testing.T) {
	tests := []struct {
		name     string
		strategy wait.Strategy
	}{
		{
			name:     "invalid HTTP mutation",
			strategy: wait.ForHTTP("/"),
		},
		{
			name:     "empty exec",
			strategy: wait.ForExec(nil),
		},
		{
			name: "nested invalid",
			strategy: wait.ForAll(
				wait.ForAny(wait.ForListeningPort("not-a-port")),
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			option := WithWaitStrategy(tc.strategy)
			if httpStrategy, ok := tc.strategy.(*wait.HTTPStrategy); ok {
				// Mutate after constructing the option to exercise Run's
				// boundary validation, not only WithWaitStrategy's eager check.
				httpStrategy.WithMethod("GET\n")
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPullPolicy(PullNever), option,
				withRunner(f), withEngine(appleEngine{}))
			if err == nil || !errors.Is(err, wait.ErrInvalidConfiguration) || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("Run error = %v, want ErrInvalidConfiguration", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called before wait validation: %v", f.calls)
			}
		})
	}
}

func TestWaitTargetClassifiesContainerNotFound(t *testing.T) {
	inspectErr := &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: `container not found: "myctr"`}
	runner := waitRunnerFunc(func(_ context.Context, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "system" {
			return nil, nil, nil
		}
		return nil, nil, inspectErr
	})
	target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}}

	_, err := target.State(context.Background())
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want root ErrContainerNotFound", err)
	}
	if !errors.Is(err, wait.ErrTargetNotFound) {
		t.Fatalf("State error = %v, want wait.ErrTargetNotFound", err)
	}
}

func TestWaitTargetReturnsUnknownWithInspectError(t *testing.T) {
	inspectErr := errors.New("temporary inspect failure")
	runner := waitRunnerFunc(func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, nil, inspectErr
	})
	target := waitTarget{c: &Container{id: "myctr", runner: runner, eng: appleEngine{}, creation: "0123456789abcdef"}}

	state, err := target.State(context.Background())
	if state != wait.StateUnknown {
		t.Errorf("State = %q, want %q", state, wait.StateUnknown)
	}
	if !errors.Is(err, inspectErr) {
		t.Errorf("State error = %v, want %v", err, inspectErr)
	}
}
