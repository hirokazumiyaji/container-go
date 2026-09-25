package container

import (
	"bytes"
	"context"
	"errors"
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

var (
	_ wait.Target      = waitTarget{}
	_ wait.StateTarget = waitTarget{}
)

func (t waitTarget) Endpoint(ctx context.Context, port string) (string, error) {
	if port == "" {
		if len(t.c.exposed) == 0 {
			return "", fmt.Errorf("no ports declared via WithExposedPorts")
		}
		port = t.c.exposed[0].String()
	}
	endpoint, err := t.c.Endpoint(ctx, port)
	return endpoint, waitTargetError(err)
}

func (t waitTarget) Running(ctx context.Context) (bool, error) {
	state, err := t.State(ctx)
	return state == wait.StateRunning, err
}

func (t waitTarget) State(ctx context.Context) (wait.State, error) {
	state, err := t.c.State(ctx)
	if err != nil {
		return wait.StateUnknown, waitTargetError(err)
	}
	switch state {
	case StateRunning:
		return wait.StateRunning, nil
	case StateStopped:
		return wait.StateStopped, nil
	case StateStopping:
		return wait.StateStopping, nil
	case StateCreated:
		return wait.StateCreated, nil
	case StateRestarting:
		return wait.StateRestarting, nil
	case StatePaused:
		return wait.StatePaused, nil
	default:
		return wait.StateUnknown, nil
	}
}

func (t waitTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	stream, err := t.c.FollowLogs(ctx)
	return stream, waitTargetError(err)
}

func (t waitTarget) ExecCommand(ctx context.Context, cmd []string) (int, error) {
	code, _, err := t.c.Exec(ctx, cmd)
	return code, waitTargetError(err)
}

func waitTargetError(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, wait.ErrTargetNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", wait.ErrTargetNotFound, err)
}

// logTailLimit bounds the diagnostic log tail attached to wait
// failures.
const logTailLimit = 1024 * 1024

// logTail fetches up to logTailLimit trailing bytes of the container's
// logs for diagnostics. It asks the backend for a bounded tail
// (logsTailArgs) and keeps only the last bytes in a fixed-size ring,
// so neither the CLI output nor the Go buffer grows with total log
// size. Failures yield an empty tail.
func (c *Container) logTail(ctx context.Context) string {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, stderr, err := c.runner.Run(qCtx, c.eng.logsTailArgs(c.id)...)
	if err != nil {
		return ""
	}
	return lastNBytes(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)), logTailLimit)
}

// lastNBytes keeps only the trailing n bytes of r using a fixed-size
// ring buffer.
func lastNBytes(r io.Reader, n int) string {
	if n <= 0 {
		_, _ = io.Copy(io.Discard, r)
		return ""
	}
	buf := make([]byte, n)
	pos := 0
	full := false
	tmp := make([]byte, 32*1024)
	for {
		m, err := r.Read(tmp)
		if m > 0 {
			chunk := tmp[:m]
			for len(chunk) > 0 {
				space := n - pos
				if len(chunk) < space {
					copy(buf[pos:], chunk)
					pos += len(chunk)
					break
				}
				copy(buf[pos:], chunk[:space])
				chunk = chunk[space:]
				pos = 0
				full = true
			}
		}
		if err != nil {
			break
		}
	}
	if !full {
		return string(buf[:pos])
	}
	out := make([]byte, n)
	copy(out, buf[pos:])
	copy(out[n-pos:], buf[:pos])
	return string(out)
}
