package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type reuseInspectFailureRunner struct {
	mu         sync.Mutex
	errs       []error
	defaultErr error
	inspects   int
}

func (r *reuseInspectFailureRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return nil, nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inspects++
	if len(r.errs) == 0 {
		if r.defaultErr != nil {
			return nil, nil, r.defaultErr
		}
		return nil, nil, errors.New("temporary inspect failure")
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return nil, nil, err
}

func (r *reuseInspectFailureRunner) inspectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inspects
}

func TestReuseDoesNotRetrySystemNotRunning(t *testing.T) {
	runner := &reuseInspectFailureRunner{errs: []error{ErrSystemNotRunning}}
	name := newContainerName()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("Run error = %v, want ErrSystemNotRunning", err)
	}
	if got := runner.inspectCount(); got != 1 {
		t.Fatalf("inspect calls = %d, want 1 (system outage is terminal)", got)
	}
}

func TestReuseDoesNotRetryContextCancellation(t *testing.T) {
	cancelErr := fmt.Errorf("inspect failed: %w", context.Canceled)
	runner := &reuseInspectFailureRunner{errs: []error{cancelErr}}
	name := newContainerName()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "inspect failed") {
		t.Fatalf("Run error = %v, want original inspect diagnostic", err)
	}
	if got := runner.inspectCount(); got != 1 {
		t.Fatalf("inspect calls = %d, want 1 (cancellation is terminal)", got)
	}
}

func TestReuseReturnsLastRealInspectError(t *testing.T) {
	permanent := errors.New("inspect permission denied")
	runner := &reuseInspectFailureRunner{errs: []error{
		errors.New("temporary inspect failure"),
		permanent,
	}}
	name := newContainerName()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, permanent) {
		t.Fatalf("Run error = %v, want last real inspect error %v", err, permanent)
	}
	if got := runner.inspectCount(); got != 2 {
		t.Fatalf("inspect calls = %d, want 2 (transient then terminal)", got)
	}
}

func TestReuseTransientInspectTimeoutPreservesLastError(t *testing.T) {
	oldPoll := reusePollInterval
	reusePollInterval = time.Millisecond
	t.Cleanup(func() { reusePollInterval = oldPoll })

	inspectErr := errors.New("temporary inspect failure")
	runner := &reuseInspectFailureRunner{defaultErr: inspectErr}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := reuseEnsureContainer(ctx, "redis:7-alpine", &config{
		name:   newContainerName(),
		eng:    appleEngine{},
		runner: runner,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, inspectErr) {
		t.Fatalf("error = %v, want last inspect error %v", err, inspectErr)
	}
	if !strings.Contains(err.Error(), "temporary inspect failure") {
		t.Fatalf("error = %v, want last inspect diagnostic", err)
	}
	if got := runner.inspectCount(); got < 2 {
		t.Fatalf("inspect calls = %d, want transient retries", got)
	}
}

func TestTransientReuseInspectErrorRejectsTerminalErrors(t *testing.T) {
	for name, err := range map[string]error{
		"system not running": fmt.Errorf("probe: %w", ErrSystemNotRunning),
		"canceled":           fmt.Errorf("inspect: %w", context.Canceled),
		"deadline":           fmt.Errorf("inspect: %w", context.DeadlineExceeded),
		"permanent":          errors.New("inspect permission denied"),
	} {
		t.Run(name, func(t *testing.T) {
			if transientReuseInspectError(err) {
				t.Fatalf("transientReuseInspectError(%v) = true, want false", err)
			}
		})
	}
}
