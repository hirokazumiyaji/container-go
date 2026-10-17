package ports

import (
	"context"
	"time"
)

// Clock provides time operations that can be replaced with a fake in unit tests.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
	SleepWithContext(ctx context.Context, d time.Duration) error
}

// RealClock implements Clock using the standard time package.
type RealClock struct{}

func (RealClock) Now() time.Time {
	return time.Now()
}

func (RealClock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (RealClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

func (RealClock) SleepWithContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
