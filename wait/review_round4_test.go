package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ForAny must honor a strategy that succeeded instead of reporting the
// timeout, so a container that became ready is not rolled back.
func TestReviewAnyStrategyHonorsLateSuccess(t *testing.T) {
	s := &AnyStrategy{startupTimeout: 50 * time.Millisecond}
	s.strategies = []Strategy{reviewSucceedStrategy{}}

	if err := s.WaitUntilReady(context.Background(), newFakeTarget()); err != nil {
		t.Fatalf("a strategy that succeeded was reported as %v", err)
	}
}

// ForAll must not turn a satisfied composite wait into a failure.
func TestReviewAllStrategyHonorsLateSuccess(t *testing.T) {
	s := &AllStrategy{startupTimeout: 50 * time.Millisecond}
	s.strategies = []Strategy{reviewSucceedStrategy{}, reviewSucceedStrategy{}}

	if err := s.WaitUntilReady(context.Background(), newFakeTarget()); err != nil {
		t.Fatalf("a satisfied ForAll was reported as %v", err)
	}
}

// reviewSucceedStrategy reports readiness immediately.
type reviewSucceedStrategy struct{}

func (reviewSucceedStrategy) WaitUntilReady(context.Context, Target) error { return nil }

// An observed readiness result must not be rewritten as a timeout when the
// deadline fires between the check returning and the context re-read.
func TestReviewPollHonorsObservedSuccessAtDeadline(t *testing.T) {
	o := options{pollInterval: time.Millisecond, startupTimeout: 30 * time.Millisecond}
	target := &flakyProbeTarget{}
	err := poll(context.Background(), o, target, "probe", func(context.Context) error {
		return nil
	}, false)
	if err != nil {
		t.Fatalf("an observed success was reported as %v", err)
	}
}

// A stopped container must be reported as stopped even when the interval
// does not divide the budget, so the reserved final probe actually runs.
func TestReviewPollRunsFinalStateProbe(t *testing.T) {
	stopped := &stoppedTarget{fakeTarget: newFakeTarget()}
	// An interval that lands iterations at 0/5/.../55s of a 60s budget would
	// never start an iteration inside the reserved final second.
	o := options{pollInterval: 5 * time.Second, startupTimeout: 5 * time.Second}
	err := poll(context.Background(), o, stopped, "probe", func(context.Context) error {
		return errors.New("not ready")
	}, false)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "container stopped") {
		t.Errorf("error = %v, want a stopped-container classification", err)
	}
}

// stoppedTarget always reports a stopped container.
type stoppedTarget struct {
	*fakeTarget
}

func (s *stoppedTarget) Running(context.Context) (bool, error) { return false, nil }
