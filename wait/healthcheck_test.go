package wait

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type healthSequenceTarget struct {
	*fakeTarget
	mu      sync.Mutex
	healths []Health
	calls   int
}

func newHealthSequenceTarget(healths ...Health) *healthSequenceTarget {
	return &healthSequenceTarget{fakeTarget: newFakeTarget(), healths: healths}
}

func (t *healthSequenceTarget) Health(context.Context) (Health, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if len(t.healths) == 0 {
		return HealthNone, nil
	}
	health := t.healths[0]
	if len(t.healths) > 1 {
		t.healths = t.healths[1:]
	}
	return health, nil
}

func (t *healthSequenceTarget) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func TestForHealthcheckWaitsForHealthy(t *testing.T) {
	target := newHealthSequenceTarget(HealthStarting, HealthStarting, HealthHealthy)

	err := ForHealthcheck().
		WithPollInterval(time.Millisecond).
		WithStartupTimeout(2*time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.callCount(); got < 3 {
		t.Fatalf("health checks = %d, want starting to be retried until healthy", got)
	}
}

func TestForHealthcheckDefaultPollDetectsHealthy(t *testing.T) {
	target := newHealthSequenceTarget(HealthStarting, HealthHealthy)

	err := ForHealthcheck().
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady with the default poll interval: %v", err)
	}
}

func TestForHealthcheckFailsWhenUnhealthy(t *testing.T) {
	target := newHealthSequenceTarget(HealthStarting, HealthUnhealthy)

	start := time.Now()
	err := ForHealthcheck().
		WithPollInterval(time.Millisecond).
		WithStartupTimeout(10*time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want an error once Docker reports unhealthy")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, unhealthy must end the wait instead of timing out", err)
	}
	if !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("error = %v, want the unhealthy verdict", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("WaitUntilReady took %v, want the terminal unhealthy result immediately", elapsed)
	}
	if got := target.callCount(); got != 2 {
		t.Errorf("health checks = %d, want one starting check and one terminal unhealthy check", got)
	}
}

func TestForHealthcheckRejectsMissingHealthcheck(t *testing.T) {
	target := newHealthSequenceTarget(HealthNone)

	err := ForHealthcheck().
		WithStartupTimeout(time.Hour).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want a configuration error when the image has no HEALTHCHECK")
	}
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %v, want *ConfigError", err)
	}
	if configErr.Strategy != "ForHealthcheck" {
		t.Errorf("ConfigError.Strategy = %q", configErr.Strategy)
	}
	if got := target.callCount(); got != 1 {
		t.Errorf("health checks = %d, want a missing HEALTHCHECK to fail without retries", got)
	}
}

func TestForHealthcheckRejectsUnsupportedTargetBeforeTargetCalls(t *testing.T) {
	target := newFakeTarget()

	err := ForHealthcheck().
		WithStartupTimeout(time.Hour).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
	}
	if got := target.stateCalls.Load(); got != 0 {
		t.Errorf("State calls = %d, want validation before any target call", got)
	}
	if got := target.endpointCalls.Load(); got != 0 {
		t.Errorf("Endpoint calls = %d, want validation before any target call", got)
	}
}

func TestRequiresHealthcheck(t *testing.T) {
	healthcheck := ForHealthcheck()
	if !RequiresHealthcheck(healthcheck) {
		t.Error("RequiresHealthcheck(ForHealthcheck()) = false")
	}
	if RequiresHealthcheck(ForHTTP("/")) {
		t.Error("RequiresHealthcheck(ForHTTP) = true")
	}
	if !RequiresHealthcheck(ForAll(ForHTTP("/"), healthcheck)) {
		t.Error("RequiresHealthcheck(ForAll with a healthcheck) = false")
	}
	if RequiresHealthcheck(ForAny(healthcheck, ForHTTP("/"))) {
		t.Error("RequiresHealthcheck(ForAny with a supported branch) = true")
	}
	if !RequiresHealthcheck(ForAny(healthcheck)) {
		t.Error("RequiresHealthcheck(ForAny with only healthchecks) = false")
	}
}
