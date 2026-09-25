//go:build !windows

package container

import (
	"context"
	"sync"
)

type nameLockRegistry struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
}

var registeredNameLocks nameLockRegistry

func acquireNameLockGate(ctx context.Context, path string) (func(), error) {
	registeredNameLocks.mu.Lock()
	if registeredNameLocks.gates == nil {
		registeredNameLocks.gates = make(map[string]chan struct{})
	}
	gate := registeredNameLocks.gates[path]
	if gate == nil {
		gate = make(chan struct{}, 1)
		registeredNameLocks.gates[path] = gate
	}
	registeredNameLocks.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
