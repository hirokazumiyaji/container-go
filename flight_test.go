package container

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightGroupSharesExecution(t *testing.T) {
	var g flightGroup[int]
	var calls atomic.Int32
	started := make(chan struct{})
	ready := make(chan struct{})

	const n = 5
	var joined atomic.Int32
	allJoined := make(chan struct{})
	g.onJoin = func(key string) {
		if joined.Add(1) == n-1 {
			close(allJoined)
		}
	}

	var wg sync.WaitGroup
	results := make([]int, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = g.do(context.Background(), "k", func() (int, error) {
				calls.Add(1)
				close(started)
				<-ready
				return 42, nil
			})
		}(i)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never started")
	}

	select {
	case <-allJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("waiters never joined in-flight entry")
	}

	close(ready)
	wg.Wait()

	if c := calls.Load(); c != 1 {
		t.Errorf("calls = %d, want 1", c)
	}
	for i, res := range results {
		if errs[i] != nil {
			t.Errorf("[%d] err = %v", i, errs[i])
		}
		if res != 42 {
			t.Errorf("[%d] res = %d, want 42", i, res)
		}
	}
}

func TestFlightGroupCallerCancellationDoesNotCancelOthers(t *testing.T) {
	var g flightGroup[string]
	started := make(chan struct{})
	block := make(chan struct{})

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() {
		_, err := g.do(ctx1, "key", func() (string, error) {
			close(started)
			<-block
			return "done", nil
		})
		done1 <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never started")
	}

	cancel1()

	select {
	case err := <-done1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first caller did not unblock on cancellation")
	}

	joined := make(chan struct{})
	g.onJoin = func(key string) {
		close(joined)
	}

	done2 := make(chan string, 1)
	go func() {
		val, _ := g.do(context.Background(), "key", func() (string, error) {
			return "unexpected", nil
		})
		done2 <- val
	}()

	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("second caller never joined in-flight entry")
	}

	close(block)

	select {
	case val := <-done2:
		if val != "done" {
			t.Errorf("second caller got %q, want %q", val, "done")
		}
	case <-time.After(time.Second):
		t.Fatal("second caller timed out")
	}
}

func TestFlightGroupRecoversFromPanic(t *testing.T) {
	var g flightGroup[int]
	_, err := g.do(context.Background(), "panic-key", func() (int, error) {
		panic("flight crashed")
	})
	if err == nil || !strings.Contains(err.Error(), "flight panic: flight crashed") {
		t.Fatalf("want panic error, got %v", err)
	}

	// Subsequent caller executes cleanly because the key was removed.
	val, err := g.do(context.Background(), "panic-key", func() (int, error) {
		return 99, nil
	})
	if err != nil || val != 99 {
		t.Fatalf("subsequent call failed: val=%d, err=%v", val, err)
	}
}
