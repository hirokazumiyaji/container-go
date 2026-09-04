package container

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightGroupSharesExecution(t *testing.T) {
	var g flightGroup[int]
	var calls atomic.Int32
	ready := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]int, 5)
	errs := make([]error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = g.do(context.Background(), "k", func() (int, error) {
				calls.Add(1)
				<-ready
				return 42, nil
			})
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
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
	block := make(chan struct{})

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() {
		_, err := g.do(ctx1, "key", func() (string, error) {
			<-block
			return "done", nil
		})
		done1 <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel1()

	select {
	case err := <-done1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first caller did not unblock on cancellation")
	}

	done2 := make(chan string, 1)
	go func() {
		val, _ := g.do(context.Background(), "key", func() (string, error) {
			return "unexpected", nil
		})
		done2 <- val
	}()

	time.Sleep(20 * time.Millisecond)
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
