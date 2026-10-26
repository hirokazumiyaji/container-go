package container

import (
	"context"
	"time"
)

// sleepWithContext sleeps for d or until ctx is done, whichever comes first.
func sleepWithContext(ctx context.Context, d time.Duration) error {
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
