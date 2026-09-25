package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hirokazumiyaji/container-go/wait"
)

// WithWaitStrategy blocks Run until the strategy reports the container
// ready. On failure the container is removed and the error carries a
// redacted tail of its logs. Built-in strategies and composites expose
// their request/command context through wait.DiagnosticValues.
func WithWaitStrategy(s wait.Strategy) Option {
	return func(c *config) error {
		c.waitStrategy = s
		c.waitDiagnosticSecrets = wait.DiagnosticValues(s)
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

// logTail fetches the backend's bounded log snapshot, redacts and sanitizes
// the complete snapshot, and only then applies the final byte cap. Taking the
// tail before redaction can split a secret at the boundary and leave its
// suffix in the diagnostic.
func (c *Container) logTail(ctx context.Context, extra ...string) string {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, stderr, err := c.runner.Run(qCtx, c.eng.logsTailArgs(c.id)...)
	if err != nil {
		return ""
	}
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)))
	safe := c.diagnosticRedactor(extra...).Text(raw.String())
	return lastNBytes(strings.NewReader(safe), logTailLimit)
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
