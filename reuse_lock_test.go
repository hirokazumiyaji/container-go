package container

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type createdRunningAdoptionRunner struct {
	mu           sync.Mutex
	inspectCalls int
	deleted      []string
	firstInspect chan struct{}
	releaseFirst chan struct{}
	releaseOnce  sync.Once
}

func (r *createdRunningAdoptionRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.inspectCalls++
		call := r.inspectCalls
		r.mu.Unlock()
		if call == 1 {
			close(r.firstInspect)
			select {
			case <-r.releaseFirst:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			return []byte(strings.ReplaceAll(ownedInspectJSONWithState("adopt", "created", true), "__CONTAINER_CREATION__", "0123456789abcdef")), nil, nil
		}
		if call == 2 {
			return []byte(strings.ReplaceAll(ownedInspectJSONWithState("adopt", "created", true), "__CONTAINER_CREATION__", "0123456789abcdef")), nil, nil
		}
		return []byte(strings.ReplaceAll(ownedInspectJSONWithState("adopt", "running", true), "__CONTAINER_CREATION__", "0123456789abcdef")), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestAppleAdoptionSerializesWithFailedCreateCleanup(t *testing.T) {
	runner := &createdRunningAdoptionRunner{
		firstInspect: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	cfg := &config{
		runner:   runner,
		eng:      appleEngine{},
		name:     "adopt",
		creation: "0123456789abcdef",
		reuse:    true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	adopted := make(chan error, 1)
	go func() {
		_, err := reuseEnsureContainer(ctx, "redis:7-alpine", cfg)
		adopted <- err
	}()
	select {
	case <-runner.firstInspect:
	case <-time.After(time.Second):
		t.Fatal("adoption did not reach first inspect")
	}

	cleanupDone := make(chan error, 1)
	go func() {
		runErr := errors.New("create failed")
		cleanupDone <- cleanupFailedCreate(ctx, cfg, runErr, runErr)
	}()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup completed while adoption held the name lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	runner.releaseOnce.Do(func() { close(runner.releaseFirst) })
	if err := <-adopted; err != nil {
		t.Fatalf("reuseEnsureContainer = %v", err)
	}

	select {
	case err := <-cleanupDone:
		if err == nil || !strings.Contains(err.Error(), "running generation") {
			t.Fatalf("cleanup error = %v, want running-generation refusal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not resume after adoption released the lock")
	}
	runner.mu.Lock()
	deleted := append([]string(nil), runner.deleted...)
	runner.mu.Unlock()
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want no deletion of the adopted generation", deleted)
	}
}
