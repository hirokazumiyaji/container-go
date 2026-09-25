package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// LogsOptions bounds a Logs snapshot. Tail keeps the last N lines
// (0 means all); Since drops entries older than the timestamp. Both
// map to the backend CLI's --tail/--since flags.
type LogsOptions struct {
	Tail  int
	Since time.Time
}

func (o LogsOptions) args() []string {
	var args []string
	if o.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(o.Tail))
	}
	if !o.Since.IsZero() {
		args = append(args, "--since", o.Since.Format(time.RFC3339))
	}
	return args
}

// Logs returns a snapshot of the container's log output so far.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	return c.LogsWithOptions(ctx, LogsOptions{})
}

// LogsWithOptions returns a bounded snapshot of the container's log
// output. Long-lived reuse containers can grow unbounded logs, so
// prefer Tail for diagnostics.
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error) {
	var stdout, stderr []byte
	err := c.withCurrentTarget(ctx, func(target string) error {
		qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
		defer cancel()
		args := c.eng.logsArgs(target, false)
		if extra := opts.args(); len(extra) > 0 {
			// Insert --tail/--since before the container ID (last arg).
			args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
		}
		var err error
		stdout, stderr, err = c.runner.Run(qCtx, args...)
		return wrapNotFound(c.classify(ctx, err))
	})
	if err != nil {
		return nil, err
	}
	// docker logs splits the container's streams across the CLI's
	// stdout and stderr; a snapshot carries both.
	return io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))), nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	var target string
	var unlock func()
	var err error
	if c.reused {
		target, _, unlock, err = c.acquireCurrentTarget(ctx)
	} else {
		target, unlock, err = c.acquireUnverifiedTarget(ctx)
	}
	if err != nil {
		return nil, err
	}
	reader, err := s.Stream(ctx, c.eng.logsArgs(target, true)...)
	if err != nil {
		unlock()
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	if usesNameAddressedDeletes(c.eng) {
		wrapped := &unlockReadCloser{
			ReadCloser: reader,
			unlock:     unlock,
			stop:       make(chan struct{}),
		}
		go func() {
			select {
			case <-ctx.Done():
				wrapped.once.Do(wrapped.unlock)
			case <-wrapped.stop:
			}
		}()
		return wrapped, nil
	}
	unlock()
	return reader, nil
}

type unlockReadCloser struct {
	io.ReadCloser
	once     sync.Once
	stopOnce sync.Once
	stop     chan struct{}
	unlock   func()
}

func (r *unlockReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.stopOnce.Do(func() { close(r.stop) })
	r.once.Do(r.unlock)
	return err
}
