package container

import (
	"context"
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
}

type flight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

func (g *flightGroup[T]) do(ctx context.Context, key string, fn func() (T, error)) (T, error) {
	g.mu.Lock()
	if g.inflight == nil {
		g.inflight = map[string]*flight[T]{}
	}
	if f, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		return g.wait(ctx, f)
	}
	f := &flight[T]{done: make(chan struct{})}
	g.inflight[key] = f
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.inflight, key)
			g.mu.Unlock()
			close(f.done)
		}()
		f.val, f.err = fn()
	}()

	return g.wait(ctx, f)
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
