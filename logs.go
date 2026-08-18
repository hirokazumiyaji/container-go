package container

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// Logs returns a snapshot of the container's log output so far.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, stderr, err := c.runner.Run(qCtx, c.eng.logsArgs(c.id, false)...)
	if err != nil {
		return nil, c.classify(ctx, err)
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
