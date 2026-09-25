//go:build !windows

package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLockNameSerializesHolders(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock while held: err = %v, want deadline exceeded", err)
	}

	unlock()
	unlock2, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestTerminateWaitsForNameLockBeforeInspecting(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	// Another process holds the name: this handle's inspect-then-delete
	// must not start until it is released, so a bounded context fails
	// before any backend call.
	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = ctr.Terminate(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Terminate = %v, want lock failure", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 while the name is locked elsewhere", r.deleteCalls)
	}
}

func TestTerminateContainerBoundsNameLockWait(t *testing.T) {
	oldTimeout := terminateTimeout
	terminateTimeout = 100 * time.Millisecond
	t.Cleanup(func() { terminateTimeout = oldTimeout })

	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	start := time.Now()
	err = TerminateContainer(ctr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TerminateContainer = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("TerminateContainer took %v, want bounded wait", elapsed)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", r.deleteCalls)
	}
}

func TestRollbackBoundsNameLockWait(t *testing.T) {
	oldTimeout := terminateTimeout
	terminateTimeout = 100 * time.Millisecond
	t.Cleanup(func() { terminateTimeout = oldTimeout })

	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	cause := errors.New("copy failed")
	start := time.Now()
	err = ctr.rollback(context.Background(), cause)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("rollback = %v, want primary and deadline errors", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("rollback took %v, want bounded wait", elapsed)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", r.deleteCalls)
	}
}
