// Package wait provides startup readiness strategies for containers.
// Apple Container has no healthcheck or wait primitive, so every
// strategy here probes from the client side.
package wait

import (
	"context"
	"fmt"
	"io"
	"time"
)

const (
	defaultStartupTimeout = 60 * time.Second
	defaultPollInterval   = 100 * time.Millisecond
	// stateCheckInterval bounds how often the fail-fast state probe
	// spawns a CLI process during polling.
	stateCheckInterval = time.Second
)

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port means the first declared
	// port.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// Strategy waits until a started container is ready for use.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

type options struct {
	startupTimeout time.Duration
	pollInterval   time.Duration
}

func (o options) effective() (timeout, interval time.Duration) {
	timeout, interval = o.startupTimeout, o.pollInterval
	if timeout == 0 {
		timeout = defaultStartupTimeout
	}
	if interval == 0 {
		interval = defaultPollInterval
	}
	return timeout, interval
}

// poll runs check every interval until it succeeds, the container
// stops, or the timeout elapses.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	timeout, interval := o.effective()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	var lastStateCheck time.Time
	for {
		if err := check(ctx); err == nil {
			return nil
		} else if ctx.Err() == nil {
			lastErr = err
		}

		if time.Since(lastStateCheck) >= stateCheckInterval {
			lastStateCheck = time.Now()
			if running, err := target.Running(ctx); err == nil && !running {
				return fmt.Errorf("%s: container stopped while waiting (last error: %v)", what, lastErr)
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: timed out after %v (last error: %v)", what, timeout, lastErr)
		case <-time.After(interval):
		}
	}
}
