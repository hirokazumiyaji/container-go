package ports

import (
	"context"
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
