package container

import (
	"context"
	"testing"
	"time"
)

func TestSleepWithContext(t *testing.T) {
	if err := sleepWithContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepWithContext = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepWithContext(ctx, time.Second); err != context.Canceled {
		t.Errorf("sleepWithContext on canceled context = %v, want context.Canceled", err)
	}
}
