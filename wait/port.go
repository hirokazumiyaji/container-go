package wait

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/portspec"
)

// HostPortStrategy waits until a TCP connection to the container's
// endpoint succeeds.
type HostPortStrategy struct {
	options
	port         string
	explicitPort bool
}

// ForListeningPort waits for the given declared port ("6379/tcp" or
// "6379") to accept TCP connections. UDP and malformed port specifications
// return a ConfigError before probing the container.
func ForListeningPort(port string) *HostPortStrategy {
	return &HostPortStrategy{port: port, explicitPort: true}
}

// ForExposedPort waits on the first port declared via
// WithExposedPorts.
func ForExposedPort() *HostPortStrategy {
	return &HostPortStrategy{}
}

func (s *HostPortStrategy) WithStartupTimeout(d time.Duration) *HostPortStrategy {
	s.startupTimeout = d
	return s
}

func (s *HostPortStrategy) WithPollInterval(d time.Duration) *HostPortStrategy {
	s.pollInterval = d
	return s
}

func (s *HostPortStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if s.explicitPort {
		if err := validateTCPPortSpec("ForListeningPort", s.port); err != nil {
			return err
		}
	}
	return poll(ctx, s.options, target, fmt.Sprintf("wait for listening port %q", s.port), func(ctx context.Context) error {
		endpoint, err := target.Endpoint(ctx, s.port)
		if err != nil {
			return err
		}
		d := net.Dialer{Timeout: time.Second}
		conn, err := d.DialContext(ctx, "tcp", endpoint)
		if err != nil {
			return err
		}
		return conn.Close()
	}, true)
}

func validateTCPPortSpec(strategy, spec string) error {
	_, err := portspec.ParseTCP(spec)
	if err == nil {
		return nil
	}
	// ConfigError.Value already carries the specification, so keep only the
	// reason rather than repeating the value in the message.
	reason := err.Error()
	if _, rest, ok := strings.Cut(reason, ": "); ok {
		reason = rest
	}
	return &ConfigError{
		Strategy: strategy,
		Field:    "port specification",
		Value:    spec,
		Reason:   reason,
	}
}
