package wait

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A readiness gate must never report success on the "wall clock passed the
// deadline but the context timer callback has not fired yet" path. The
// pattern was never observed, so the wait has to fail.
func TestReviewLogWaitFailsWhenBudgetExpiredWithoutTimer(t *testing.T) {
	// A context whose Deadline is in the past but whose Err() is still nil
	// reproduces the asynchronous-timer window exactly.
	callerCtx := context.Background()
	waitCtx := &expiredButLiveContext{deadline: time.Now().Add(-time.Second)}

	err := handleLogScanResult(callerCtx, waitCtx, time.Second, "log pattern", nil, logScanResult{found: false})
	if err == nil {
		t.Fatal("expired budget reported success for an unobserved pattern")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired budget should classify as a timeout, got %v", err)
	}
}

// expiredButLiveContext has a past deadline but reports no error, matching
// the window in which the runtime has not yet run the timer callback.
type expiredButLiveContext struct {
	deadline time.Time
}

func (c *expiredButLiveContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *expiredButLiveContext) Done() <-chan struct{}       { return nil }
func (c *expiredButLiveContext) Err() error                  { return nil }
func (c *expiredButLiveContext) Value(any) any               { return nil }
