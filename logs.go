package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// LogsOptions configures a Logs snapshot. Tail keeps the last N lines
// (0 means all); Since drops entries older than the timestamp. Both
// map to the backend CLI's --tail/--since flags. A positive MaxBytes
// caps the combined stdout+stderr response; zero preserves the
// historical unbounded snapshot behavior.
type LogsOptions struct {
	Tail     int
	Since    time.Time
	MaxBytes int64
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
//
// Deprecated: use LogsWithOptions with a positive MaxBytes or LogsTo for
// output that may be large or untrusted. This compatibility form keeps
// the historical unbounded capture.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	return c.LogsWithOptions(ctx, LogsOptions{})
}

// LogsWithOptions returns a snapshot of the container's log output. A
// positive opts.MaxBytes enables a fixed-size capture; zero retains the
// historical full-snapshot behavior during the compatibility period.
// Bounded captures merge stdout and stderr in arrival order. Prefer a
// limit for untrusted or long-lived container output.
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error) {
	if opts.MaxBytes < 0 {
		return nil, fmt.Errorf("logs: MaxBytes must be non-negative, got %d", opts.MaxBytes)
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	args := c.eng.logsArgs(c.id, false)
	if extra := opts.args(); len(extra) > 0 {
		// Insert --tail/--since before the container ID (last arg).
		args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
	}
	if opts.MaxBytes > 0 {
		output := newBoundedBuffer(opts.MaxBytes)
		_, err := cli.RunTo(c.runner, qCtx, output, output, args...)
		if err != nil {
			return nil, wrapNotFound(c.classify(ctx, err))
		}
		return &boundedReadCloser{boundedReader: output.reader()}, nil
	}
	stdout, stderr, err := c.runner.Run(qCtx, args...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	// docker logs splits the container's streams across the CLI's
	// stdout and stderr; a snapshot carries both.
	return io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))), nil
}

// LogsTo streams a log snapshot directly to output. When MaxBytes is
// positive, bytes after the limit are drained and discarded rather than
// buffered. The returned stats count all bytes observed by the CLI.
func (c *Container) LogsTo(ctx context.Context, output io.Writer, opts LogsOptions) (OutputStats, error) {
	if opts.MaxBytes < 0 {
		return OutputStats{}, fmt.Errorf("logs: MaxBytes must be non-negative, got %d", opts.MaxBytes)
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	args := c.eng.logsArgs(c.id, false)
	if extra := opts.args(); len(extra) > 0 {
		args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
	}

	var sink io.Writer
	var limited *limitedWriter
	if opts.MaxBytes > 0 {
		limited = limitedStreamWriter(output, opts.MaxBytes)
		sink = limited
	} else {
		sink = streamWriter(output)
	}
	stats, err := cli.RunTo(c.runner, qCtx, sink, sink, args...)
	result := OutputStats{
		Bytes:     stats.StdoutBytes + stats.StderrBytes,
		Truncated: stats.Truncated(),
	}
	if limited != nil {
		result.Truncated = result.Truncated || limited.Truncated()
	}
	if err != nil {
		return result, wrapNotFound(c.classify(ctx, err))
	}
	return result, nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	return s.Stream(ctx, c.eng.logsArgs(c.id, true)...)
}
