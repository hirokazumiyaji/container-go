package ports

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRealClock(t *testing.T) {
	c := RealClock{}
	before := time.Now()
	now := c.Now()
	if now.Before(before) {
		t.Errorf("Now() = %v, before %v", now, before)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SleepWithContext(ctx, time.Second); err != context.Canceled {
		t.Errorf("SleepWithContext on canceled context = %v, want context.Canceled", err)
	}
}

// FakeClock provides a deterministic Clock implementation for tests.
type FakeClock struct {
	mu       sync.Mutex
	notifyMu sync.Mutex
	now      time.Time
	waiters  []*fakeWaiter
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
	// Serialize advances so timers fire in deadline order, but do not
	// send while holding mu: a blocked send would stall Now/After.
	f.notifyMu.Lock()
	defer f.notifyMu.Unlock()

	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	var fired []chan time.Time
	var remaining []*fakeWaiter
	for _, w := range f.waiters {
		if !w.target.After(now) {
			fired = append(fired, w.ch)
		} else {
			remaining = append(remaining, w)
		}
	}
	f.waiters = remaining
	f.mu.Unlock()

	for _, ch := range fired {
		ch <- now
	}
}

// WaitersCount returns the number of pending timers.
func (f *FakeClock) WaitersCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

func TestFakeClock(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := NewFakeClock(start)

	if got := fc.Now(); !got.Equal(start) {
		t.Errorf("Now() = %v, want %v", got, start)
	}

	ch := fc.After(10 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired before Advance")
	default:
	}

	if fc.WaitersCount() != 1 {
		t.Errorf("WaitersCount() = %d, want 1", fc.WaitersCount())
	}

	fc.Advance(5 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired too early")
	default:
	}

	fc.Advance(5 * time.Second)
	select {
	case firedAt := <-ch:
		want := start.Add(10 * time.Second)
		if !firedAt.Equal(want) {
			t.Errorf("firedAt = %v, want %v", firedAt, want)
		}
	default:
		t.Fatal("timer did not fire after 10s")
	}

	if fc.WaitersCount() != 0 {
		t.Errorf("WaitersCount() = %d, want 0", fc.WaitersCount())
	}

	// SleepWithContext
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fc.SleepWithContext(ctx, time.Minute); err != context.Canceled {
		t.Errorf("SleepWithContext = %v, want context.Canceled", err)
	}

	ctx2 := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- fc.SleepWithContext(ctx2, time.Minute)
	}()
	for fc.WaitersCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	fc.Advance(time.Minute)
	if err := <-done; err != nil {
		t.Errorf("SleepWithContext unexpected error: %v", err)
	}
}
