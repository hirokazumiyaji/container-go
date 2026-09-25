package container

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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
	// Readiness probes discard application output. ExecTo drains the
	// CLI streams into io.Discard instead of materializing a byte slice
	// for every polling attempt.
	code, _, err := t.c.ExecTo(ctx, cmd, io.Discard)
	return code, err
}

// logTailLimit bounds the diagnostic log tail attached to wait
// failures.
const logTailLimit = 1024 * 1024

// logTail fetches up to logTailLimit trailing bytes of the container's
// logs for diagnostics. It asks the backend for a bounded tail
// (logsTailArgs) and streams both CLI streams through a fixed-size
// ring, so neither the CLI output nor the Go buffer grows with total
// log size. Failures yield an empty tail.
func (c *Container) logTail(ctx context.Context) string {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	tail := newTailWriter(logTailLimit)
	_, err := cli.RunTo(c.runner, qCtx, tail, tail, c.eng.logsTailArgs(c.id)...)
	if err != nil {
		return ""
	}
	return tail.String()
}

// lastNBytes keeps only the trailing n bytes of r using a fixed-size
// ring buffer.
func lastNBytes(r io.Reader, n int) string {
	if n <= 0 {
		_, _ = io.Copy(io.Discard, r)
		return ""
	}
	tail := newTailWriter(n)
	_, _ = io.Copy(tail, r)
	return tail.String()
}

type tailWriter struct {
	mu   sync.Mutex
	buf  []byte
	pos  int
	full bool
}

func newTailWriter(n int) *tailWriter {
	return &tailWriter{buf: make([]byte, n)}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(p)
	if len(w.buf) == 0 {
		return original, nil
	}
	for len(p) > 0 {
		space := len(w.buf) - w.pos
		if len(p) < space {
			copy(w.buf[w.pos:], p)
			w.pos += len(p)
			break
		}
		copy(w.buf[w.pos:], p[:space])
		p = p[space:]
		w.pos = 0
		w.full = true
	}
	return original, nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.full {
		return string(w.buf[:w.pos])
	}
	out := make([]byte, len(w.buf))
	copy(out, w.buf[w.pos:])
	copy(out[len(w.buf)-w.pos:], w.buf[:w.pos])
	return string(out)
}
