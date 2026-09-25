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

// FollowLogs streams the container's log output until Close is called,
// the context is cancelled, or the stream reaches end of file. Close
// terminates the underlying CLI process. A generation-bound Apple handle
// holds its name lock for the whole stream and releases it as soon as the
// stream can no longer observe the container.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	var target string
	var unlock func()
	var err error
	// FollowLogs is a long-lived operation. Reuse handles and every
	// generation-bound Apple handle must verify the current name before
	// opening the stream; only legacy unbound diagnostic handles retain
	// the unverified one-call behavior.
	target, unlock, err = c.acquireHandleTarget(ctx)
	if err != nil {
		return nil, err
	}
	reader, err := s.Stream(ctx, c.eng.logsArgs(target, true)...)
	if err != nil {
		unlock()
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	if reader == nil {
		// A runner that reports success without a stream would leave the
		// name lock held for a log source that can never be read or
		// closed, blocking every other path for that name.
		unlock()
		return nil, errors.New("logs: streaming runner returned no stream")
	}
	if usesNameAddressedDeletes(c.eng) {
		wrapped := newUnlockReadCloser(reader, unlock)
		go wrapped.watchCancellation(ctx)
		return wrapped, nil
	}
	unlock()
	return reader, nil
}

type unlockReadCloser struct {
	io.ReadCloser
	closeOnce  sync.Once
	closeDone  chan struct{}
	closeErr   error
	unlockOnce sync.Once
	stopOnce   sync.Once
	stop       chan struct{}
	unlock     func()
}

func newUnlockReadCloser(reader io.ReadCloser, unlock func()) *unlockReadCloser {
	return &unlockReadCloser{
		ReadCloser: reader,
		closeDone:  make(chan struct{}),
		stop:       make(chan struct{}),
		unlock:     unlock,
	}
}

// closeStream serializes underlying Close calls and waits for the first
// one to finish. The Apple name lock must remain held until the backend
// stream has been closed and its child/process wait has completed.
func (r *unlockReadCloser) closeStream() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.ReadCloser.Close()
		close(r.closeDone)
	})
	<-r.closeDone
	return r.closeErr
}

func (r *unlockReadCloser) releaseLock() {
	r.unlockOnce.Do(r.unlock)
}

// Read releases the name lock on a terminal read. A caller that drains
// the stream to end of file, or hits a stream error, must not need a
// separate Close call to let a create, cleanup, or terminate proceed on
// the same name. Non-terminal reads are returned untouched and the lock
// is still held.
func (r *unlockReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		// Close the backend stream first: the name lock may only be
		// released after the CLI process has stopped observing it.
		_ = r.closeStream()
		r.stopOnce.Do(func() { close(r.stop) })
		r.releaseLock()
	}
	return n, err
}

func (r *unlockReadCloser) watchCancellation(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = r.closeStream()
		r.releaseLock()
	case <-r.stop:
	}
}

func (r *unlockReadCloser) Close() error {
	err := r.closeStream()
	r.stopOnce.Do(func() { close(r.stop) })
	r.releaseLock()
	return err
}
