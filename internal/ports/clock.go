package ports

import (
	"context"
	"sync"
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
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// FakeClock provides a deterministic Clock implementation for tests.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	target time.Time
	ch     chan time.Time
}

// NewFakeClock returns a FakeClock initialized to the given time.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, &fakeWaiter{
		target: f.now.Add(d),
		ch:     ch,
	})
	return ch
}

func (f *FakeClock) Sleep(d time.Duration) {
	<-f.After(d)
}

func (f *FakeClock) removeWaiter(ch <-chan time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, w := range f.waiters {
		if w.ch == ch {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			return
		}
	}
}

func (f *FakeClock) SleepWithContext(ctx context.Context, d time.Duration) error {
	ch := f.After(d)
	select {
	case <-ctx.Done():
		f.removeWaiter(ch)
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// Advance advances the fake clock by d and fires all eligible timers.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)

	var remaining []*fakeWaiter
	for _, w := range f.waiters {
		if !w.target.After(f.now) {
			w.ch <- f.now
		} else {
			remaining = append(remaining, w)
		}
	}
	f.waiters = remaining
}

// WaitersCount returns the number of pending timers.
func (f *FakeClock) WaitersCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}
