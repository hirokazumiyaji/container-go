package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
)

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
