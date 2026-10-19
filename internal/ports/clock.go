package ports

import (
	"context"
	"time"
)

// RealClock provides context-aware time operations using the standard library.
type RealClock struct{}

// SleepWithContext sleeps for d or until ctx is done, whichever comes first.
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
