package wait

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ForAny must name the strategies that failed. waitError.Error renders only
// the message, so a bare terminal message would hide every child cause from a
// caller reading the error string.
func TestReviewAnyTerminalErrorNamesChildFailures(t *testing.T) {
	terminal := newWaitError("wait for any: startup timeout 60s elapsed", context.DeadlineExceeded)
	child := errors.New("log pattern never observed")

	err := anyTerminalError(terminal, []error{child})
	if !strings.Contains(err.Error(), child.Error()) {
		t.Errorf("child failure missing from message: %q", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("terminal cause lost: %v", err)
	}
}

// A transient probe failure must not be reported once a later probe succeeds.
func TestReviewWaitClearsStaleProbeError(t *testing.T) {
	sentinel := errors.New("transient probe failure")
	target := &flakyProbeTarget{failFirst: sentinel}

	// check always fails, so poll runs to its startup timeout while the
	// target probes recover after the first failure. The window must exceed
	// stateCheckInterval so a second probe actually runs.
	o := options{pollInterval: time.Millisecond, startupTimeout: stateCheckInterval + 500*time.Millisecond}
	check := func(context.Context) error { return errors.New("not ready") }
	err := poll(context.Background(), o, target, "probe", check, true)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if errors.Is(err, sentinel) {
		t.Errorf("stale probe error survived a later successful probe: %v", err)
	}
}

// flakyProbeTarget fails its first Running probe with a sentinel, then
// reports running.
type flakyProbeTarget struct {
	fakeTarget
	mu        sync.Mutex
	failFirst error
	calls     int
}

func (t *flakyProbeTarget) Running(context.Context) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return false, t.failFirst
	}
	return true, nil
}
