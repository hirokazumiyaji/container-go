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
	stdout, _, err := c.runner.Run(qCtx, "logs", c.id)
	if err != nil {
		return nil, cli.Classify(ctx, c.runner, err)
	}
	return io.NopCloser(bytes.NewReader(stdout)), nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	return s.Stream(ctx, "logs", "--follow", c.id)
}
