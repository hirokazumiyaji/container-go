package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// WithWaitStrategy blocks Run until the strategy reports the container
// ready. On failure, a non-reuse Run attempts to remove the container
// and, when available, attaches a bounded tail of its logs; a reuse wait
// leaves the shared container in place.
func WithWaitStrategy(s wait.Strategy) Option {
	return func(c *config) error {
		if err := wait.Validate(s); err != nil {
			return err
		}
		c.waitStrategy = s
		c.waitDiagnosticSecrets = wait.DiagnosticValues(s)
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
		// Port probes are TCP-only. A UDP declaration must not silently
		// become the target of an implicit ForExposedPort/ForHTTP probe.
		// Exposed declarations retain their order even when another port
		// has an explicit host binding; published-only containers fall back
		// to their first TCP binding.
		for _, exposed := range t.c.exposed {
			if exposed.proto == "tcp" {
				port = exposed.String()
				break
			}
		}
		if port == "" {
			for _, published := range t.c.published {
				if published.proto == "tcp" {
					port = strconv.Itoa(published.containerPort) + "/" + published.proto
					break
				}
			}
		}
		if port == "" {
			return "", ErrPortNotExposed
		}
	}
	endpoint, err := t.c.Endpoint(ctx, port)
	return endpoint, waitTargetError(err)
}

func (t waitTarget) Running(ctx context.Context) (bool, error) {
	state, err := t.State(ctx)
	return state == wait.StateRunning, err
}

func (t waitTarget) State(ctx context.Context) (wait.State, error) {
	if t.c == nil || t.c.eng == nil {
		return wait.StateRunning, nil
	}
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
	// Readiness probes discard application output. ExecTo drains the CLI
	// streams into io.Discard instead of materializing a buffer for every
	// polling attempt.
	code, _, err := t.c.ExecTo(ctx, cmd, io.Discard)
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

// logTailWithError fetches up to logTailLimit trailing bytes of the
// verified operation target's logs for diagnostics. It asks the backend
// for a bounded tail (logsTailArgs) and keeps only the last bytes in a
// fixed-size ring, so neither the CLI output nor the Go buffer grows with
// total log size. Resolving the target first keeps a deleted or replaced
// same-name container from contributing unrelated logs to the error.
func (c *Container) logTailWithError(ctx context.Context, extra ...string) (string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(qCtx)
	if err != nil {
		return "", err
	}
	return c.logTailTarget(qCtx, target, extra...)
}

func (c *Container) logTailTarget(ctx context.Context, target string, extra ...string) (string, error) {
	redactor := c.diagnosticRedactor(extra...)
	if streamer, ok := c.runner.(cli.SnapshotStreamer); ok {
		stream, err := streamer.StreamSnapshot(ctx, c.eng.logsTailArgs(target)...)
		if err != nil {
			return "", err
		}
		defer stream.Close()
		return redactor.RedactTail(stream, logTailLimit)
	}
	stdout, stderr, err := c.runner.Run(ctx, c.eng.logsTailArgs(target)...)
	if err != nil {
		return "", err
	}
	return redactor.RedactTail(
		io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)),
		logTailLimit,
	)
}
