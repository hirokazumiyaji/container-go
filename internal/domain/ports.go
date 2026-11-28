package domain

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// PortSpec declares a container port and transport protocol. It is the single
// representation of a parsed "port[/proto]" specification shared by the root
// package, the wait package, and the backend adapters.
type PortSpec struct {
	Port  int
	Proto string
}

func (p PortSpec) String() string {
	return strconv.Itoa(p.Port) + "/" + p.Proto
}

// ParsePortSpec parses a "port" or "port/proto" specification. A missing
// protocol defaults to tcp; the protocol comparison is case-sensitive,
// matching the grammar the public API documents.
func ParsePortSpec(s string) (PortSpec, error) {
	portPart, proto, ok := strings.Cut(s, "/")
	if !ok {
		proto = protocolTCP
	}
	if proto != protocolTCP && proto != protocolUDP {
		return PortSpec{}, fmt.Errorf("invalid port %q: protocol must be tcp or udp", s)
	}
	return finishPortSpec(s, portPart, proto)
}

// ParseTCPPortSpec is ParsePortSpec restricted to TCP. Its reasons are phrased
// for a TCP-only caller, because "only TCP is supported" and "protocol must be
// tcp" are more useful there than the generic message ParsePortSpec produces
// for any non-TCP scheme.
func ParseTCPPortSpec(s string) (PortSpec, error) {
	portPart, proto, ok := strings.Cut(s, "/")
	if !ok {
		proto = protocolTCP
	}
	if proto != protocolTCP {
		reason := "protocol must be tcp"
		if proto == protocolUDP {
			reason = "only TCP is supported"
		}
		return PortSpec{}, fmt.Errorf("invalid port %q: %s", s, reason)
	}
	return finishPortSpec(s, portPart, proto)
}

const (
	protocolTCP = "tcp"
	protocolUDP = "udp"
)

func finishPortSpec(s, portPart, proto string) (PortSpec, error) {
	n, err := parsePortNumber(portPart)
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid port %q: port must be 1-65535", s)
	}
	return PortSpec{Port: n, Proto: proto}, nil
}

// PublishSpec declares a host-to-container port mapping.
type PublishSpec struct {
	// HostAddr is the parsed host bind address, stored unmapped by
	// ParsePublishSpec. The zero value means the caller did not request a
	// specific address.
	HostAddr      netip.Addr
	HostPort      int
	ContainerPort int
	Proto         string
	Raw           string
}

// ConnectAddr returns the connect address for this publish specification.
// An unspecified bind (0.0.0.0 or ::) maps to the same address family's
// loopback so a client does not dial the wildcard address.
func (p PublishSpec) ConnectAddr() string {
	addr := p.HostAddr
	if !addr.IsValid() {
		return "127.0.0.1"
	}
	if addr.IsUnspecified() {
		if addr.Is4() {
			return "127.0.0.1"
		}
		return "::1"
	}
	return addr.String()
}

// HostIP returns the host bind address as a canonical string, or "" when the
// specification does not request one. It gives reuse keys a stable spelling
// that does not depend on the netip zero value's rendering.
func (p PublishSpec) HostIP() string {
	if !p.HostAddr.IsValid() {
		return ""
	}
	return p.HostAddr.String()
}

// fieldError associates a parse failure with the public option field it came
// from, so the root package can rebuild its ValidationError contract.
type fieldError struct {
	field   string
	message string
}

func (e *fieldError) Error() string { return e.message }

func newFieldError(field, message string) error {
	return &fieldError{field: field, message: message}
}

// FieldOf returns the public option field for a parse error that belongs to
// one, when the parser knows it.
func FieldOf(err error) (string, bool) {
	var fieldErr *fieldError
	if errors.As(err, &fieldErr) {
		return fieldErr.field, true
	}
	return "", false
}

// ParsePublishSpec parses a publish specification string
// "[hostAddr:]hostPort:containerPort[/proto]". A bracketed host address must
// be a literal IPv6 address, and an explicitly empty host address is
// malformed: it would render an empty bind address rather than asking for the
// default one.
func ParsePublishSpec(s string) (PublishSpec, error) {
	spec := PublishSpec{Raw: s, Proto: protocolTCP}
	rest := s
	if portsPart, proto, ok := strings.Cut(s, "/"); ok {
		if proto != protocolTCP && proto != protocolUDP {
			return PublishSpec{}, newFieldError("published port", "protocol must be tcp or udp")
		}
		spec.Proto = proto
		rest = portsPart
	}

	hostAddr := ""
	hostExplicit := false
	bracketed := false
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]:")
		if end < 0 {
			return PublishSpec{}, newFieldError("published port", "bracketed IPv6 form is malformed")
		}
		hostAddr = rest[1:end]
		hostExplicit = true
		bracketed = true
		rest = rest[end+2:]
	} else if parts := strings.Split(rest, ":"); len(parts) == 3 {
		hostAddr = parts[0]
		hostExplicit = true
		rest = parts[1] + ":" + parts[2]
	}
	if hostExplicit {
		if hostAddr == "" {
			return PublishSpec{}, newFieldError("published port", "host address must not be empty")
		}
		addr, err := netip.ParseAddr(hostAddr)
		if err != nil {
			return PublishSpec{}, fmt.Errorf("invalid publish spec %q: host address must be an IP: %w", s, err)
		}
		if bracketed && (!addr.Is6() || addr.Zone() != "") {
			return PublishSpec{}, fmt.Errorf("invalid publish spec %q: bracketed host address must be an IPv6 address", s)
		}
		spec.HostAddr = addr.Unmap()
	}

	hostPart, ctrPart, ok := strings.Cut(rest, ":")
	if !ok || strings.Contains(ctrPart, ":") {
		return PublishSpec{}, newFieldError("published port", "want [host-ip:]host-port:container-port[/proto]")
	}
	var err error
	if spec.HostPort, err = parsePortNumber(hostPart); err != nil {
		return PublishSpec{}, newFieldError("port", "must be 1-65535")
	}
	if spec.ContainerPort, err = parsePortNumber(ctrPart); err != nil {
		return PublishSpec{}, newFieldError("port", "must be 1-65535")
	}
	return spec, nil
}

// parsePortNumber parses a decimal port number and ensures it is in 1-65535.
// Only ASCII digits are accepted: strconv.Atoi would also take a leading
// "+", which is not part of the port grammar.
func parsePortNumber(s string) (int, error) {
	if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("invalid port %q: must be 1-65535", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q: must be 1-65535", s)
	}
	return n, nil
}

// CanonicalIP parses an address and returns its unmapped string representation.
func CanonicalIP(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	return ip.Unmap().String()
}

// IPIs4 reports whether addr is an IPv4 address.
func IPIs4(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().Is4()
}

// IPIsLoopback reports whether addr is a loopback address.
func IPIsLoopback(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().IsLoopback()
}

// IPIsUnspecified reports whether addr is an unspecified (0.0.0.0 or ::) address.
func IPIsUnspecified(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().IsUnspecified()
}

// IsLoopbackOrUnspecified reports whether addr represents localhost or unspecified binds.
func IsLoopbackOrUnspecified(addr string) bool {
	if addr == "" || strings.EqualFold(addr, "localhost") {
		return true
	}
	return IPIsLoopback(addr) || IPIsUnspecified(addr)
}
