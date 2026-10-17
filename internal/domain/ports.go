package domain

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/portspec"
)

// PortSpec declares a container port and transport protocol.
type PortSpec struct {
	Port  int
	Proto string
}

func (p PortSpec) String() string {
	return strconv.Itoa(p.Port) + "/" + p.Proto
}

// ParsePortSpec parses a "port[/proto]" string.
func ParsePortSpec(s string) (PortSpec, error) {
	spec, err := portspec.Parse(s)
	if err != nil {
		return PortSpec{}, err
	}
	return PortSpec{Port: spec.Port, Proto: spec.Protocol}, nil
}

// PublishSpec declares a host-to-container port mapping.
type PublishSpec struct {
	HostAddr      string
	HostPort      int
	ContainerPort int
	Proto         string
	Raw           string
}

// ConnectAddr returns the connect address for this publish specification.
func (p PublishSpec) ConnectAddr() string {
	if p.HostAddr == "" {
		return "127.0.0.1"
	}
	if IPIsUnspecified(p.HostAddr) {
		if IPIs4(p.HostAddr) {
			return "127.0.0.1"
		}
		return "::1"
	}
	return CanonicalIP(p.HostAddr)
}

// ParsePublishSpec parses a publish specification string "[hostAddr:][hostPort:]containerPort[/proto]".
func ParsePublishSpec(s string) (PublishSpec, error) {
	spec := PublishSpec{Raw: s, Proto: "tcp"}
	rest := s
	if portsPart, proto, ok := strings.Cut(s, "/"); ok {
		if proto != "tcp" && proto != "udp" {
			return PublishSpec{}, fmt.Errorf("invalid published port %q: protocol must be tcp or udp", s)
		}
		spec.Proto = proto
		rest = portsPart
	}

	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]:")
		if end < 0 {
			return PublishSpec{}, fmt.Errorf("invalid published port %q: bracketed IPv6 form is malformed", s)
		}
		spec.HostAddr = rest[1:end]
		rest = rest[end+2:]
	} else if parts := strings.Split(rest, ":"); len(parts) == 3 {
		spec.HostAddr = parts[0]
		rest = parts[1] + ":" + parts[2]
	}
	if spec.HostAddr != "" {
		addr, err := netip.ParseAddr(spec.HostAddr)
		if err != nil {
			return PublishSpec{}, fmt.Errorf("invalid publish spec %q: host address must be an IP: %w", s, err)
		}
		spec.HostAddr = addr.Unmap().String()
	}

	hostPart, ctrPart, ok := strings.Cut(rest, ":")
	if !ok || strings.Contains(ctrPart, ":") {
		return PublishSpec{}, fmt.Errorf("invalid published port %q: want [host-ip:]host-port:container-port[/proto]", s)
	}
	var err error
	if spec.HostPort, err = ParsePortNumber(hostPart); err != nil {
		return PublishSpec{}, err
	}
	if spec.ContainerPort, err = ParsePortNumber(ctrPart); err != nil {
		return PublishSpec{}, err
	}
	return spec, nil
}

// ParsePortNumber parses a port number and ensures it is in 1-65535.
func ParsePortNumber(s string) (int, error) {
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

// IPIs6 reports whether addr is an IPv6 address.
func IPIs6(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().Is6()
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
