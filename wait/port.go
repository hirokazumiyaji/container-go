package wait

import (
	"context"
	"fmt"
	"net"
	"time"
)

// HostPortStrategy waits until a TCP connection to the container's
// endpoint succeeds.
type HostPortStrategy struct {
	options
	port    string
	portSet bool
}

// ForListeningPort waits for the given declared port ("6379/tcp" or
// "6379") to accept TCP connections.
func ForListeningPort(port string) *HostPortStrategy {
	return &HostPortStrategy{port: port, portSet: true}
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

func (s *HostPortStrategy) validate() error {
	if err := s.options.validate(); err != nil {
		return err
	}
	return validateTCPPortSpec(s.port, !s.portSet)
}

func (s *HostPortStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := s.validate(); err != nil {
		return err
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
