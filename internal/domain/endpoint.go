package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Network modes whose reported value changes how a host endpoint is derived.
// A backend that does not report a mode leaves EndpointNetwork.Mode empty.
const (
	NetworkModeHost = "host"
	NetworkModeNone = "none"
)

// PortBinding is one host-side binding of a container port as reported by
// inspect. A binding is usable only when HostPort is positive.
type PortBinding struct {
	ContainerPort int
	Proto         string
	HostAddr      string
	HostPort      int
}

// EndpointNetwork is the inspect-derived network surface endpoint resolution
// depends on. It deliberately uses no backend types so the resolution rules
// can be unit-tested without a container backend.
type EndpointNetwork struct {
	// IP is the container's first reported address. A direct-IP backend
	// dials it when no publish specification is present.
	IP string
	// Mode is the network mode inspect reported: Docker's "host" or "none",
	// or empty when the backend reports no mode.
	Mode string
	// Bindings lists the host-side port bindings inspect reported.
	Bindings []PortBinding
}

// EndpointBackend describes how the selected backend exposes container ports.
type EndpointBackend struct {
	// DirectIP reports that clients connect to the container address instead
	// of a backend host binding.
	DirectIP bool
	// Remote reports that the daemon runs on another machine, so a loopback
	// binding is not reachable from this client.
	Remote bool
	// DefaultHost is the client-facing backend address used when no container
	// address or host binding applies.
	DefaultHost string
}

// EndpointRequest is the port configuration a handle was asked about.
type EndpointRequest struct {
	// Container identifies the handle in resolution errors.
	Container string
	// Port is the container port Endpoint and MappedPort resolve. Its zero
	// value resolves the host only, mirroring Host.
	Port PortSpec
	// Published is the publish specification matched to Port, or the first
	// publish specification for host-only resolution. It is nil when the
	// caller requested no publish.
	Published *PublishSpec
}

var (
	// ErrPortNotExposed identifies a requested port that has no usable host
	// binding. The wait and container packages alias this value for their
	// public sentinel.
	ErrPortNotExposed = errors.New("port is not declared or has no usable host binding")
	// ErrNoReachableHost identifies a network mode that has no host endpoint.
	// The container package aliases this value for its public sentinel.
	ErrNoReachableHost = errors.New("container has no reachable host")
	// ErrEndpointUnreachable identifies an inspected host binding this client
	// cannot reach, such as loopback on a remote daemon. The container package
	// aliases this value for its public sentinel.
	ErrEndpointUnreachable = errors.New("container endpoint is unreachable")

	// errNoReportedAddress classifies a direct-IP snapshot without a
	// container address. It has no public sentinel: Host and Endpoint expose
	// the plain message and the caller retries after re-inspecting.
	errNoReportedAddress = errors.New("container address is not reported")
)

// endpointError carries the classification and detail of a resolution
// failure. stale marks a failure that an incomplete inspect snapshot can
// explain, so the shell clears the snapshot and re-inspects on the next read.
type endpointError struct {
	kind  error
	stale bool
	text  string
}

func (e *endpointError) Error() string { return e.text }

func (e *endpointError) Unwrap() error { return e.kind }

func endpointFailure(kind error, stale bool, format string, args ...any) error {
	return &endpointError{kind: kind, stale: stale, text: kind.Error() + ": " + fmt.Sprintf(format, args...)}
}

func endpointDetail(kind error, stale bool, format string, args ...any) error {
	return &endpointError{kind: kind, stale: stale, text: fmt.Sprintf(format, args...)}
}

// EndpointSnapshotStale reports whether err came from an inspect snapshot
// that lacked the address or binding needed to answer, rather than from a
// durable configuration or reachability failure. The imperative shell clears
// its dynamic snapshot for such an error so the next read re-inspects.
func EndpointSnapshotStale(err error) bool {
	var resolutionErr *endpointError
	return errors.As(err, &resolutionErr) && resolutionErr.stale
}

// ResolveEndpoint derives the client-facing host and port for one container
// from inspect-equivalent network data and the ports the caller requested. It
// is pure: the shell performs the identity-checked inspect, default-network
// setup, and cache invalidation, then applies the returned values and any
// diagnostic redaction its method contract requires.
//
// A zero req.Port resolves the host only (Host): a direct-IP backend returns
// its published address or container IP; Docker host networking returns the
// daemon host, Docker none networking fails, and a backend with host bindings
// returns the first published binding or the default host.
//
// A nonzero req.Port resolves Endpoint and MappedPort: the port must be
// declared by the shell, a matching published specification takes priority
// over an exposed declaration, and a direct-IP backend returns the published
// address and port or the container address and port. Docker host and none
// modes cannot infer a service port and fail with ErrPortNotExposed (none also
// fails with ErrNoReachableHost). An unspecified IPv4 or IPv6 bind normalizes
// to its address family's loopback, and a loopback host binding on a remote
// daemon fails with ErrEndpointUnreachable.
func ResolveEndpoint(net EndpointNetwork, backend EndpointBackend, req EndpointRequest) (string, int, error) {
	// Inspect does not pad the mode, but the exported NetworkEndpointError also
	// trims it; normalize once so both paths classify the same value.
	net.Mode = strings.TrimSpace(net.Mode)
	hostOnly := req.Port.Port == 0
	if backend.DirectIP {
		if req.Published != nil {
			if hostOnly {
				return req.Published.ConnectAddr(), 0, nil
			}
			return req.Published.ConnectAddr(), req.Published.HostPort, nil
		}
		if net.IP == "" {
			return "", 0, endpointDetail(errNoReportedAddress, true, "container %s has no reported IP address", req.Container)
		}
		if hostOnly {
			return net.IP, 0, nil
		}
		return net.IP, req.Port.Port, nil
	}
	if hostOnly {
		return resolveHostEndpoint(net, backend, req)
	}
	return resolvePortEndpoint(net, backend, req)
}

func resolveHostEndpoint(net EndpointNetwork, backend EndpointBackend, req EndpointRequest) (string, int, error) {
	switch net.Mode {
	case NetworkModeNone:
		return "", 0, endpointFailure(ErrNoReachableHost, false, "container %s uses network mode %q", req.Container, net.Mode)
	case NetworkModeHost:
		if req.Published != nil {
			// Docker discards -p in host mode, so a requested publish is not
			// a binding and Endpoint cannot infer a port for it either.
			return "", 0, NetworkEndpointError(net.Mode)
		}
		return backend.DefaultHost, 0, nil
	}
	if req.Published == nil {
		return backend.DefaultHost, 0, nil
	}
	binding, ok := MatchingPublishedBinding(net.Bindings, *req.Published, backend.Remote)
	if !ok {
		return "", 0, endpointFailure(ErrPortNotExposed, true, "published port %q has no host binding in network mode %q", req.Published.Raw, net.Mode)
	}
	host, err := BindingConnectHost(binding, backend)
	if err != nil {
		return "", 0, err
	}
	return host, 0, nil
}

func resolvePortEndpoint(net EndpointNetwork, backend EndpointBackend, req EndpointRequest) (string, int, error) {
	if err := NetworkEndpointError(net.Mode); err != nil {
		return "", 0, err
	}
	if req.Published != nil {
		// Published over exposed is the priority rule: a requested publish
		// that inspect does not confirm must not silently fall back to an
		// exposed binding with a different host port.
		binding, ok := MatchingPublishedBinding(net.Bindings, *req.Published, backend.Remote)
		if !ok {
			return "", 0, endpointFailure(ErrPortNotExposed, true, "published port %q has no host binding in network mode %q", req.Published.Raw, net.Mode)
		}
		host, err := BindingConnectHost(binding, backend)
		if err != nil {
			return "", 0, err
		}
		return host, binding.HostPort, nil
	}
	binding, ok := MatchingExposedBinding(net.Bindings, req.Port, backend.Remote)
	if !ok {
		return "", 0, endpointFailure(ErrPortNotExposed, true, "%s has no host binding in network mode %q", req.Port, net.Mode)
	}
	host, err := BindingConnectHost(binding, backend)
	if err != nil {
		return "", 0, err
	}
	return host, binding.HostPort, nil
}

// MatchingPublishedBinding selects the inspected binding a requested publish
// specification names. It is the single selection rule shared by endpoint
// resolution and reuse port compatibility. A remote daemon's loopback bind is
// retained only as a fallback: returning it lets the connect-host check report
// why it cannot be reached instead of pretending the published port is absent.
func MatchingPublishedBinding(bindings []PortBinding, p PublishSpec, remote bool) (PortBinding, bool) {
	var loopback PortBinding
	foundLoopback := false
	for _, b := range bindings {
		if b.ContainerPort != p.ContainerPort || b.Proto != p.Proto || b.HostPort <= 0 {
			continue
		}
		if p.HostPort != 0 && b.HostPort != p.HostPort {
			continue
		}
		if p.HostIP() != "" && CanonicalIP(b.HostAddr) != p.HostIP() {
			continue
		}
		if remote && IPIsLoopback(b.HostAddr) {
			loopback, foundLoopback = b, true
			continue
		}
		return b, true
	}
	return loopback, foundLoopback
}

// MatchingExposedBinding selects the inspected binding of an exposed port. It
// is shared by endpoint resolution and reuse port compatibility. On a remote
// daemon a loopback binding is kept as a fallback for the same reason as in
// MatchingPublishedBinding.
func MatchingExposedBinding(bindings []PortBinding, spec PortSpec, remote bool) (PortBinding, bool) {
	var loopback PortBinding
	foundLoopback := false
	for _, b := range bindings {
		if b.ContainerPort != spec.Port || b.Proto != spec.Proto || b.HostPort <= 0 {
			continue
		}
		if remote && IPIsLoopback(b.HostAddr) {
			loopback, foundLoopback = b, true
			continue
		}
		return b, true
	}
	return loopback, foundLoopback
}

// BindingConnectHost validates a selected binding for this client and
// normalizes its address. Endpoint resolution and reuse reachability both use
// it.
func BindingConnectHost(binding PortBinding, backend EndpointBackend) (string, error) {
	if backend.Remote && IPIsLoopback(binding.HostAddr) {
		return "", endpointFailure(ErrEndpointUnreachable, false, "Docker port %d is bound to remote-daemon loopback %q", binding.HostPort, CanonicalIP(binding.HostAddr))
	}
	return ConnectHost(binding.HostAddr, backend), nil
}

// ConnectHost maps an inspected bind address to the address a client
// dials. An empty host is the backend default; on a remote daemon, loopback
// and unspecified addresses become the remote host; locally, unspecified IPv4
// and IPv6 become the matching loopback while an explicit loopback is kept.
func ConnectHost(addr string, backend EndpointBackend) string {
	canonical := CanonicalIP(addr)
	if canonical == "" {
		return backend.DefaultHost
	}
	if backend.Remote && IsLoopbackOrUnspecified(canonical) {
		return backend.DefaultHost
	}
	if IPIsUnspecified(canonical) {
		if IPIs4(canonical) {
			return "127.0.0.1"
		}
		return "::1"
	}
	return canonical
}

// NetworkEndpointError reports a port resolution failure caused by a reported
// network mode that cannot create a host port binding. It returns nil for any
// other mode.
func NetworkEndpointError(mode string) error {
	switch strings.TrimSpace(mode) {
	case NetworkModeHost:
		return endpointFailure(ErrPortNotExposed, false, "Docker host networking has no library-managed host port binding; Host returns the daemon host, but Endpoint does not infer a port")
	case NetworkModeNone:
		return errors.Join(
			endpointFailure(ErrPortNotExposed, false, "Docker network mode %q has no network interface for a host port binding", mode),
			endpointFailure(ErrNoReachableHost, false, "Docker network mode %q has no reachable host", mode),
		)
	default:
		return nil
	}
}
