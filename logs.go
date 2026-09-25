package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// LogsOptions describes an optional window for a log snapshot. Tail is
// intended to keep the last N lines (0 means all); the current checkout
// does not reject a negative Tail and treats it as 0/all (#102). Since
// drops entries older than the timestamp. The current checkout translates
// both options to the Docker-style --tail/--since arguments for either
// engine. Docker supports those flags. Apple Container 1.2.x–1.3.x uses
// -n for a tail but has no --since option; issue #82 is intended to map
// Tail to -n and reject Since with an explicit unsupported-capability
// result. Until #82 is applied, non-empty LogsWithOptions options are
// reliable for Docker only.
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

// LogsWithOptions returns a finite snapshot of the container's log
// output. The options are backend-specific; see LogsOptions. On this
// checkout Apple Container still receives Docker-style option names, so
// Apple support is pending issue #82. A snapshot is not byte-bounded, and
// long-lived reuse containers can grow without a requested window.
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	args := c.eng.logsArgs(c.id, false)
	if extra := opts.args(); len(extra) > 0 {
		// Insert --tail/--since before the container ID (last arg).
		args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
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
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	return s.Stream(ctx, c.eng.logsArgs(c.id, true)...)
}
