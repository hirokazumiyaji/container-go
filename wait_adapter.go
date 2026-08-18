package container

import (
	"context"
	"fmt"
	"io"

	"github.com/hirokazumiyaji/container-go/wait"
)

// WithWaitStrategy blocks Run until the strategy reports the container
// ready. On failure the container is removed and the error carries a
// tail of its logs.
func WithWaitStrategy(s wait.Strategy) Option {
	return func(c *config) error {
		c.waitStrategy = s
		return nil
	}
}

// waitTarget adapts *Container to wait.Target.
type waitTarget struct {
	c *Container
}

func (t waitTarget) Endpoint(ctx context.Context, port string) (string, error) {
	if port == "" {
		if len(t.c.exposed) == 0 {
			return "", fmt.Errorf("no ports declared via WithExposedPorts")
		}
		port = t.c.exposed[0].String()
	}
	return t.c.Endpoint(ctx, port)
}

func (t waitTarget) Running(ctx context.Context) (bool, error) {
	state, err := t.c.State(ctx)
	if err != nil {
		return false, err
	}
	return state == StateRunning, nil
}

func (t waitTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	return t.c.FollowLogs(ctx)
}

func (t waitTarget) ExecCommand(ctx context.Context, cmd []string) (int, error) {
	code, _, err := t.c.Exec(ctx, cmd)
	return code, err
}

// logTailLimit bounds the diagnostic log tail attached to wait
// failures.
const logTailLimit = 1024 * 1024

// logTail fetches up to logTailLimit trailing bytes of the container's
// logs for diagnostics. Failures yield an empty tail.
func (c *Container) logTail(ctx context.Context) string {
	rc, err := c.Logs(ctx)
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, logTailLimit+1))
	if err != nil {
		return ""
	}
	if len(data) > logTailLimit {
		data = data[len(data)-logTailLimit:]
	}
	return string(data)
}
