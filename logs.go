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
	target, unlock, err := c.verifiedOperationTargetWithSharedLock(ctx)
	if err != nil {
		return nil, err
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	args := c.eng.logsArgs(target, false)
	if extra := opts.args(); len(extra) > 0 {
		// Insert --tail/--since before the container ID (last arg).
		args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
	}
	stdout, stderr, err := c.runner.Run(qCtx, args...)
	if err != nil {
		unlock()
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	// docker logs splits the container's streams across the CLI's
	// stdout and stderr; a snapshot carries both.
	reader := io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)))
	return newLockedReadCloser(ctx, reader, unlock), nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	target, unlock, err := c.verifiedOperationTargetWithSharedLock(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := s.Stream(ctx, c.eng.logsArgs(target, true)...)
	if err != nil {
		unlock()
		return nil, err
	}
	// The shared generation pin blocks replacement writers while allowing
	// concurrent State/Endpoint readers, including wait.ForAny probes.
	return newLockedReadCloser(ctx, stream, unlock), nil
}

// lockedReadCloser keeps the shared generation pin for the complete stream
// lifetime. Close is idempotent and also releases the pin on cancellation.
type lockedReadCloser struct {
	io.ReadCloser
	unlock func()
	done   chan struct{}
	once   sync.Once
	err    error
}

func newLockedReadCloser(ctx context.Context, stream io.ReadCloser, unlock func()) io.ReadCloser {
	r := &lockedReadCloser{ReadCloser: stream, unlock: unlock, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-r.done:
		}
	}()
	return r
}

func (r *lockedReadCloser) Close() error {
	r.once.Do(func() {
		r.err = r.ReadCloser.Close()
		close(r.done)
		r.unlock()
	})
	return r.err
}
