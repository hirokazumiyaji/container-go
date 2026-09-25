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
	port string
}

// ForListeningPort waits for the given declared port ("6379/tcp" or
// "6379") to accept TCP connections. It is TCP-only. The current
// implementation does not reject UDP or malformed specifications before
// probing: malformed specifications are retried, while a UDP specification
// is passed to a TCP dial until the wait ends (#77).
func ForListeningPort(port string) *HostPortStrategy {
	return &HostPortStrategy{port: port}
}

// ForExposedPort waits on the first port declared via
// WithExposedPorts. It shares the TCP-only implementation: if the first
// declaration is UDP, the current probe may dial it as TCP (#77).
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
