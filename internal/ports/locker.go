package ports

import "context"

// Locker provides synchronization over named resources.
type Locker interface {
	Lock(ctx context.Context) (unlock func(), err error)
}
