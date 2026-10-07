package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// LogsOptions bounds a Logs snapshot. Tail keeps the last N lines
// (0 means all); Since drops entries older than the timestamp. Each
// backend maps the options to its supported CLI arguments.
type LogsOptions struct {
	Tail  int
	Since time.Time
}

// Logs returns a snapshot of the container's log output so far.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	return c.LogsWithOptions(ctx, LogsOptions{})
}

// LogsWithOptions returns a bounded snapshot of the container's log
// output. Long-lived reuse containers can grow unbounded logs, so
// prefer Tail for diagnostics. It returns ErrUnsupportedCapability
// without invoking the backend when the backend cannot honor opts.
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	target, err := c.verifiedOperationTarget(qCtx)
	if err != nil {
		return nil, err
	}
	args, err := c.eng.logsArgsWithOptions(target, opts)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := c.runner.Run(qCtx, args...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	// docker logs splits the container's streams across the CLI's
	// stdout and stderr; a snapshot carries both.
	return io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))), nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	target, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return nil, err
	}
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	return s.Stream(ctx, c.eng.logsFollowArgs(target)...)
}
