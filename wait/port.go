package wait

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrInvalidPort reports a port declaration that is not valid
	// syntax. It is a configuration error, not a readiness failure.
	ErrInvalidPort = errors.New("invalid port")
	// ErrUnsupportedProtocol reports a port declaration whose protocol
	// this strategy cannot probe. ForListeningPort dials TCP, so a
	// UDP-only service is rejected instead of consuming a whole
	// startup timeout on a dial that can never succeed.
	ErrUnsupportedProtocol = errors.New("unsupported port protocol")
)

// HostPortStrategy waits until a TCP connection to the container's
// endpoint succeeds.
type HostPortStrategy struct {
	options
	port string
}

// ForListeningPort waits for the given declared port ("6379/tcp" or
// "6379") to accept TCP connections. A "/udp" declaration fails fast
// with ErrUnsupportedProtocol.
func ForListeningPort(port string) *HostPortStrategy {
	return &HostPortStrategy{port: port}
}

// ForExposedPort waits on the first port declared via
// WithExposedPorts. The declaration is resolved by the container, so a
// UDP-first container is not rejected here; the strategy still dials
// TCP only.
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
	if err := s.validatePort(); err != nil {
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

// validatePort rejects a declaration this strategy cannot probe before
// polling starts, so a misconfigured port costs no startup timeout. The
// grammar mirrors the container's own port parsing; the two are kept in
// step by the invalid-syntax cases below rather than by sharing code,
// which this package cannot do. An empty port means "the first declared
// port", which only the container can resolve, so it is left alone.
func (s *HostPortStrategy) validatePort() error {
	if s.port == "" {
		return nil
	}
	portPart, proto, ok := strings.Cut(s.port, "/")
	if !ok {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return fmt.Errorf("wait for listening port %q: %w: protocol must be tcp or udp", s.port, ErrInvalidPort)
	}
	n, err := strconv.Atoi(portPart)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("wait for listening port %q: %w: port must be 1-65535", s.port, ErrInvalidPort)
	}
	if proto == "udp" {
		return fmt.Errorf("wait for listening port %q: %w: this strategy dials TCP only", s.port, ErrUnsupportedProtocol)
	}
	return nil
}
