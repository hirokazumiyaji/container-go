// Package portspec parses the "port[/proto]" specifications the public API
// accepts for exposed, published, and waited-on ports.
//
// It exists because both the root container package and the wait package need
// the same grammar, and wait cannot import container: the root package already
// imports wait, so sharing through it would be an import cycle. Keeping one
// parser here means a widening of the grammar cannot be applied to one caller
// and forgotten in the other.
package portspec

import (
	"fmt"
	"strconv"
	"strings"
)

// Spec is a parsed port specification.
type Spec struct {
	Port     int
	Protocol string
}

// Protocols recognized by Parse.
const (
	TCP = "tcp"
	UDP = "udp"
)

// Parse reads a "port" or "port/proto" specification. A missing protocol
// defaults to tcp; the protocol comparison is case-sensitive, matching the
// grammar the public API documents.
func Parse(s string) (Spec, error) {
	portPart, proto, ok := strings.Cut(s, "/")
	if !ok {
		proto = TCP
	}
	if proto != TCP && proto != UDP {
		return Spec{}, fmt.Errorf("invalid port %q: protocol must be tcp or udp", s)
	}
	n, err := strconv.Atoi(portPart)
	if err != nil || n < 1 || n > 65535 {
		return Spec{}, fmt.Errorf("invalid port %q: port must be 1-65535", s)
	}
	return Spec{Port: n, Protocol: proto}, nil
}

// ParseTCP is Parse restricted to TCP. Its reasons are phrased for a TCP-only
// caller, because "only TCP is supported" and "protocol must be tcp" are more
// useful there than the generic message Parse produces for any non-TCP scheme.
func ParseTCP(s string) (Spec, error) {
	portPart, proto, ok := strings.Cut(s, "/")
	if !ok {
		proto = TCP
	}
	if proto != TCP {
		reason := "protocol must be tcp"
		if proto == UDP {
			reason = "only TCP is supported"
		}
		return Spec{}, fmt.Errorf("invalid port %q: %s", s, reason)
	}
	n, err := strconv.Atoi(portPart)
	if err != nil || n < 1 || n > 65535 {
		return Spec{}, fmt.Errorf("invalid port %q: port must be 1-65535", s)
	}
	return Spec{Port: n, Protocol: proto}, nil
}
