package wait

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/portspec"
)

// PortListenStatus is the result of a container-side TCP listen check.
type PortListenStatus int

const (
	// PortListenUnsupported means the target cannot inspect the
	// container's sockets. Strategies then accept the host connection
	// alone.
	PortListenUnsupported PortListenStatus = iota
	// PortListenClosed means no container-side socket is in LISTEN state
	// for the port yet.
	PortListenClosed
	// PortListenOpen means a container-side socket is in LISTEN state.
	PortListenOpen
)

// PortListenTarget is an optional extension to Target for backends whose
// published host endpoint can accept a TCP connection before the container
// process listens (Docker's userland proxy or Docker Desktop's port
// forwarder). Built-in port strategies consult it after a successful host
// connection and fall back to that connection when the target reports
// PortListenUnsupported. That fallback is best-effort: it restores the
// historical host-only behavior because the container-side check is
// unavailable, not because the port is known to be ready.
type PortListenTarget interface {
	// ContainerPortStatus reports whether the declared TCP port ("6379" or
	// "6379/tcp") is in LISTEN state inside the container. An empty port
	// asks the target for the same implicit selection as Endpoint. It
	// returns PortListenUnsupported, with a nil error, when the backend or
	// image cannot run the check; those targets must not fail the wait.
	ContainerPortStatus(ctx context.Context, port string) (PortListenStatus, error)
}

// HostPortStrategy waits until a TCP connection to the container's
// endpoint succeeds and, when the target implements PortListenTarget, the
// declared container port is in LISTEN state.
type HostPortStrategy struct {
	options
	port     string
	portSet  bool
	hostOnly bool
}

// ForListeningPort waits for the given declared port ("6379/tcp" or
// "6379") to accept TCP connections. UDP and malformed port specifications
// return a ConfigError before probing the container.
//
// A successful host connection is not sufficient on Docker: docker-proxy
// or Docker Desktop's port forwarder completes the handshake before the
// container process listens. The strategy therefore also checks the
// declared container port via PortListenTarget. Apple Container and images
// where the check cannot run (for example distroless images without cat)
// fall back to the host connection alone; that fallback is best-effort. Use
// WithHostOnly to skip the container-side check explicitly.
func ForListeningPort(port string) *HostPortStrategy {
	return &HostPortStrategy{port: port, portSet: true}
}

// ForExposedPort waits on the first TCP port declared via
// WithExposedPorts, falling back to the first published TCP port when
// the container has no exposed TCP port. On Docker it also verifies that
// the selected container port is in LISTEN state through PortListenTarget,
// with the same fallback as ForListeningPort; use WithHostOnly to skip
// that check.
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

// WithHostOnly skips the container-side listen check and accepts a
// successful host connection alone. The check is already skipped
// automatically when the target reports PortListenUnsupported.
func (s *HostPortStrategy) WithHostOnly() *HostPortStrategy {
	s.hostOnly = true
	return s
}

func (s *HostPortStrategy) DiagnosticValues() []string { return []string{s.port} }

func (s *HostPortStrategy) validate() error {
	if err := s.options.validate(); err != nil {
		return err
	}
	if s.portSet {
		return validateTCPPortSpec("ForListeningPort", s.port)
	}
	return nil
}

func (s *HostPortStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	defer func() { retErr = safeDiagnosticError(retErr, s.DiagnosticValues()...) }()
	if err := s.validate(); err != nil {
		return err
	}
	// One dialer shared by every poll iteration; it holds no
	// per-connection state, so rebuilding it per iteration only
	// wasted an allocation.
	d := net.Dialer{Timeout: time.Second}
	return poll(ctx, s.options, target, fmt.Sprintf("wait for listening port %q", s.port), func(ctx context.Context) error {
		endpoint, err := target.Endpoint(ctx, s.port)
		if err != nil {
			return err
		}
		conn, err := d.DialContext(ctx, "tcp", endpoint)
		if err != nil {
			return err
		}
		if err := conn.Close(); err != nil {
			return err
		}
		return s.confirmContainerListening(ctx, target)
	})
}

// confirmContainerListening rejects a host connection that precedes the
// container process. The check is best-effort: an absent optional interface
// or an unsupported target keeps the historical host-only behavior.
func (s *HostPortStrategy) confirmContainerListening(ctx context.Context, target Target) error {
	if s.hostOnly {
		return nil
	}
	listenTarget, ok := target.(PortListenTarget)
	if !ok {
		return nil
	}
	status, err := listenTarget.ContainerPortStatus(ctx, s.port)
	if err != nil {
		return err
	}
	if status == PortListenOpen || status == PortListenUnsupported {
		return nil
	}
	if s.port == "" {
		return errors.New("selected container port is not listening yet")
	}
	return fmt.Errorf("container port %q is not listening yet", s.port)
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
