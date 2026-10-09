package container

import (
	"context"
	"fmt"
	"sync"
)

// flightGroup collapses concurrent calls with the same key into one
// execution: the first caller starts fn, later callers wait for its
// result. Any caller's cancelled context returns without disturbing the
// execution or the other waiters. Entries are always removed on
// completion so a failed flight is retried on the next call.
type flightGroup[T any] struct {
	mu       sync.Mutex
	inflight map[string]*flight[T]
	onJoin   func(key string) // test hook: invoked when a waiter joins an in-flight key
}

type flight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

func (g *flightGroup[T]) do(ctx context.Context, key string, fn func() (T, error)) (T, error) {
	value, _, err := g.doWithLeader(ctx, key, fn)
	return value, err
}

// doWithLeader is the role-aware form used when a successful shared result
// can carry a warning. The bool is true only for the caller that created the
// in-flight entry; all callers that joined it receive false. The result and
// error are still shared exactly as in do.
func (g *flightGroup[T]) doWithLeader(ctx context.Context, key string, fn func() (T, error)) (T, bool, error) {
	g.mu.Lock()
	if g.inflight == nil {
		g.inflight = map[string]*flight[T]{}
	}
	if f, ok := g.inflight[key]; ok {
		onJoin := g.onJoin
		g.mu.Unlock()
		if onJoin != nil {
			onJoin(key)
		}
		value, err := g.wait(ctx, f)
		return value, false, err
	}
	f := &flight[T]{done: make(chan struct{})}
	g.inflight[key] = f
	g.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				f.err = fmt.Errorf("flight panic: %v", r)
			}
			g.mu.Lock()
			delete(g.inflight, key)
			g.mu.Unlock()
			close(f.done)
		}()
		f.val, f.err = fn()
	}()

	value, err := g.wait(ctx, f)
	return value, true, err
}

func (g *flightGroup[T]) wait(ctx context.Context, f *flight[T]) (T, error) {
	var zero T
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// doErr is a convenience for error-only flights (image pulls).
func doErr(ctx context.Context, g *flightGroup[struct{}], key string, fn func() error) error {
	_, err := g.do(ctx, key, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}
