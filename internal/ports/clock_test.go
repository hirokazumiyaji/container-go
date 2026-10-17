package ports

import (
	"context"
	"testing"
	"time"
)

func TestRealClockSleepWithContext(t *testing.T) {
	if err := (RealClock{}).SleepWithContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("SleepWithContext = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (RealClock{}).SleepWithContext(ctx, time.Second); err != context.Canceled {
		t.Errorf("SleepWithContext on canceled context = %v, want context.Canceled", err)
	}
}
