// Package wait provides startup readiness strategies for containers.
// Apple Container has no healthcheck or wait primitive, so every
// strategy here probes from the client side.

package wait

import (
	"context"
	"io"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

const (
	defaultStartupTimeout = 60 * time.Second
	defaultPollInterval   = 100 * time.Millisecond
	stateCheckInterval    = time.Second
	lifecycleProbeTimeout = 5 * time.Second
)

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port asks the built-in adapter for
	// its first exposed TCP declaration, falling back to a published TCP
	// declaration.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream. A
	// terminal CLI failure is returned by Read after the stream starts.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// State is a container lifecycle state reported by Target. It aliases the
// domain type so a target can return the state it already holds instead of
// mapping between two identical enumerations.
type State = domain.State

const (
	StateUnknown    State = domain.StateUnknown
	StateCreated    State = domain.StateCreated
	StateRunning    State = domain.StateRunning
	StateStopping   State = domain.StateStopping
	StateStopped    State = domain.StateStopped
	StateRestarting State = domain.StateRestarting
	StatePaused     State = domain.StatePaused
)

// StateTarget is an optional extension to Target for targets that can
// distinguish transitional states (Created, Restarting, Unknown) from
// terminal ones (Stopped, Stopping, Paused). Built-in strategies use it
// when available and fall back to Running.
type StateTarget interface {
	State(ctx context.Context) (State, error)
}

// Health is a Docker HEALTHCHECK status reported by Target. Its zero value
// means the image defines no HEALTHCHECK.
type Health string

const (
	HealthNone      Health = ""
	HealthStarting  Health = "starting"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

// HealthTarget is an optional extension to Target for targets that can
// report Docker HEALTHCHECK status. ForHealthcheck returns a ConfigError for
// targets that do not implement it.
type HealthTarget interface {
	Health(ctx context.Context) (Health, error)
}

// Strategy waits until a started container is ready for use.
// Implementations must return when ctx is done so composite strategies can
// collect their final causes without extending the caller's budget.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

type options struct {
	startupTimeout time.Duration
	pollInterval   time.Duration
	// stateFirst runs the lifecycle probe before the readiness check when
	// it is due. Healthcheck waits use it so the 1 Hz inspect that refreshes
	// the health snapshot happens before the check reads that snapshot.
	stateFirst bool
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
