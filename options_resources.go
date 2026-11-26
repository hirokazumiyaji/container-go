package container

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// WithCPUs sets the CPU quota for the container's VM. Fractional values such
// as 0.5 are passed to Docker as --cpus. Apple Container accepts only whole
// numbers and rejects a fractional value with ErrUnsupportedCapability before
// invoking its CLI. Non-positive and non-finite values are rejected.
func WithCPUs(v float64) Option {
	return func(c *config) error {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return validationErrorf("WithCPUs", v, "cpus must be finite, got %v", v)
		}
		if v <= 0 {
			return validationErrorf("WithCPUs", v, "cpus must be greater than zero, got %s", formatCPUs(v))
		}
		c.cpus = v
		return nil
	}
}

// formatCPUs renders a CPU quota in the form passed to the backend.
func formatCPUs(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// WithMemory sets the VM memory size, e.g. "512M" or "1G".
func WithMemory(size string) Option {
	return func(c *config) error {
		parsed, err := domain.ParseByteSize(size)
		if err != nil {
			return newValidationError("WithMemory", size, err)
		}
		c.memory = parsed
		return nil
	}
}

// WithShmSize sets the size of /dev/shm, e.g. "64M" or "1G". It uses the same
// integer with an optional K/M/G/T/P suffix grammar as WithMemory. Apple
// Container's --shm-size and Docker's --shm-size accept the value directly.
func WithShmSize(size string) Option {
	return func(c *config) error {
		parsed, err := domain.ParseByteSize(size)
		if err != nil {
			return newValidationError("WithShmSize", size, err)
		}
		c.shmSize = parsed
		return nil
	}
}

// WithPrivileged runs the container with extended privileges (Docker's
// --privileged). Apple Container has no equivalent capability and rejects it
// with ErrUnsupportedCapability before invoking its CLI.
func WithPrivileged() Option {
	return func(c *config) error {
		c.privileged = true
		return nil
	}
}

// capabilityRE is Docker's and Apple Container's accepted capability spelling:
// uppercase letters, digits, and underscores with an optional CAP_ prefix. The
// special value ALL adds or drops every capability.
var capabilityRE = regexp.MustCompile(`^(?:CAP_)?[A-Z][A-Z0-9_]*$`)

// validateCapability accepts Docker's capability spelling. The CAP_ prefix is
// optional; the caller's spelling is preserved in argv because both Docker and
// Apple Container accept either form.
func validateCapability(option, capability string) error {
	invalid := func(format string, args ...any) error {
		return newValidationErrorWithField(option, "capability", capability, fmt.Errorf(format, args...))
	}
	if capability == "" {
		return invalid("capability must not be empty")
	}
	if capability == "ALL" {
		return nil
	}
	if capability == "CAP_ALL" {
		return invalid("invalid capability %q: use ALL without the CAP_ prefix", capability)
	}
	if capability == "CAP_" || !capabilityRE.MatchString(capability) {
		return invalid("invalid capability %q: use uppercase letters, digits, and underscores with an optional CAP_ prefix", capability)
	}
	return nil
}

// capabilityKey is the prefixed spelling used to detect duplicates and to
// compare requested capabilities with inspected ones.
func capabilityKey(capability string) string {
	if capability == "ALL" || strings.HasPrefix(capability, "CAP_") {
		return capability
	}
	return "CAP_" + capability
}

// WithCapAdd adds Linux capabilities (for example NET_ADMIN or CAP_NET_ADMIN).
// The CAP_ prefix is optional and the caller's spelling is passed to the
// backend. Duplicates are ignored regardless of prefix, so repeated options are
// idempotent. Apple Container supports --cap-add with the same forms.
func WithCapAdd(caps ...string) Option {
	return func(c *config) error {
		added, err := appendCapabilities("WithCapAdd", c.capAdd, caps)
		if err != nil {
			return err
		}
		c.capAdd = added
		return nil
	}
}

// WithCapDrop drops Linux capabilities. Names follow WithCapAdd's rules.
func WithCapDrop(caps ...string) Option {
	return func(c *config) error {
		dropped, err := appendCapabilities("WithCapDrop", c.capDrop, caps)
		if err != nil {
			return err
		}
		c.capDrop = dropped
		return nil
	}
}

func appendCapabilities(option string, existing, caps []string) ([]string, error) {
	normalized := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(existing)+len(caps))
	for _, capability := range existing {
		seen[capabilityKey(capability)] = struct{}{}
	}
	for _, capability := range caps {
		if err := validateCapability(option, capability); err != nil {
			return nil, err
		}
		key := capabilityKey(capability)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, capability)
	}
	return normalized, nil
}

// ulimitNames is the resource-name set both Docker and Apple Container parse
// in their --ulimit flag. Names are lowercase and have no RLIMIT_ prefix.
var ulimitNames = map[string]struct{}{
	"core": {}, "cpu": {}, "data": {}, "fsize": {}, "locks": {},
	"memlock": {}, "msgqueue": {}, "nice": {}, "nofile": {}, "nproc": {},
	"rss": {}, "rtprio": {}, "rttime": {}, "sigpending": {}, "stack": {},
}

// ulimitSpec is one --ulimit entry in backend-neutral form.
type ulimitSpec struct {
	name       string
	soft, hard int64
}

func (u ulimitSpec) arg() string {
	return fmt.Sprintf("%s=%d:%d", u.name, u.soft, u.hard)
}

// WithUlimit sets a resource limit using Docker's name=soft:hard syntax.
// Supported names are core, cpu, data, fsize, locks, memlock, msgqueue, nice,
// nofile, nproc, rss, rtprio, rttime, sigpending, and stack. The value -1 means
// unlimited. Values below -1 and a soft limit above the hard limit are rejected,
// as is a duplicate name.
// Apple Container parses the same names and syntax.
func WithUlimit(name string, soft, hard int64) Option {
	return func(c *config) error {
		if _, ok := ulimitNames[name]; !ok {
			return newValidationErrorWithField("WithUlimit", "name", name,
				fmt.Errorf("invalid ulimit name %q", name))
		}
		if soft < -1 {
			return newValidationErrorWithField("WithUlimit", "soft", soft,
				fmt.Errorf("ulimit %q soft limit must be -1 (unlimited) or non-negative: %d", name, soft))
		}
		if hard < -1 {
			return newValidationErrorWithField("WithUlimit", "hard", hard,
				fmt.Errorf("ulimit %q hard limit must be -1 (unlimited) or non-negative: %d", name, hard))
		}
		if hard != -1 && (soft == -1 || soft > hard) {
			return newValidationErrorWithField("WithUlimit", "soft", soft,
				fmt.Errorf("ulimit %q soft limit %d must not exceed hard limit %d", name, soft, hard))
		}
		for _, existing := range c.ulimits {
			if existing.name == name {
				return newValidationErrorWithField("WithUlimit", "name", name,
					fmt.Errorf("duplicate ulimit %q", name))
			}
		}
		c.ulimits = append(c.ulimits, ulimitSpec{name: name, soft: soft, hard: hard})
		return nil
	}
}

// userRE allows "name", "uid", or "uid:gid" style users.
var userRE = regexp.MustCompile(`^[a-zA-Z0-9._][a-zA-Z0-9._-]*(:[a-zA-Z0-9._-]+)?$`)

// WithUser sets the user ("name" or "uid[:gid]") for the init process.
func WithUser(u string) Option {
	return func(c *config) error {
		if !userRE.MatchString(u) {
			return validationErrorf("WithUser", u, "invalid user %q", u)
		}
		c.user = u
		return nil
	}
}
