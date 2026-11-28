package container

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

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

// WithReadOnlyRootFS mounts the container's root filesystem read-only
// (Docker and Apple Container --read-only). Writable paths must be supplied
// with WithMounts.
func WithReadOnlyRootFS() Option {
	return func(c *config) error {
		c.readOnlyRootFS = true
		return nil
	}
}

// WithInit runs a lightweight init process as the container's first process
// (Docker and Apple Container --init). The init forwards signals and reaps
// orphaned child processes.
func WithInit() Option {
	return func(c *config) error {
		c.initProcess = true
		return nil
	}
}

// WithDNS adds custom DNS nameservers. Each server is passed as its own
// --dns flag, and every value must be a literal IPv4 or IPv6 address.
// Duplicate entries are preserved because resolver order can matter. Both
// Docker and Apple Container accept repeated --dns flags.
func WithDNS(servers ...string) Option {
	return func(c *config) error {
		for _, server := range servers {
			parsed := net.ParseIP(server)
			if parsed == nil {
				return newValidationErrorWithField("WithDNS", "server", server,
					fmt.Errorf("invalid DNS server %q: must be an IPv4 or IPv6 address", server))
			}
			c.dnsServers = append(c.dnsServers, parsed.String())
		}
		return nil
	}
}

// stopSignalNames is the set of Linux signal names a stop signal accepts
// without the SIG prefix. The aliases IOT, CLD, and POLL are the traditional
// names for ABRT, CHLD, and IO. Docker's signal parser understands the same
// set, with the SIG prefix optional.
var stopSignalNames = map[string]struct{}{
	"ABRT": {}, "ALRM": {}, "BUS": {}, "CHLD": {}, "CLD": {}, "CONT": {},
	"FPE": {}, "HUP": {}, "ILL": {}, "INT": {}, "IO": {}, "IOT": {},
	"KILL": {}, "PIPE": {}, "POLL": {}, "PROF": {}, "PWR": {}, "QUIT": {},
	"SEGV": {}, "STKFLT": {}, "STOP": {}, "SYS": {}, "TERM": {}, "TRAP": {},
	"TSTP": {}, "TTIN": {}, "TTOU": {}, "URG": {}, "USR1": {}, "USR2": {},
	"VTALRM": {}, "WINCH": {}, "XCPU": {}, "XFSZ": {},
}

// maxStopSignalNumber is the highest realtime signal number Linux defines.
const maxStopSignalNumber = 64

// validStopSignal reports whether signal is one of the spellings a container
// stop signal accepts: a positive signal number in Linux's 1..64 range, a
// standard signal name, or a realtime spelling such as SIGRTMIN+3 or
// SIGRTMAX-1. A leading sign, whitespace, control character, or flag-shaped
// value is not accepted, and neither is a non-realtime name with an offset.
func validStopSignal(signal string) bool {
	if number, err := strconv.Atoi(signal); err == nil {
		return number >= 1 && number <= maxStopSignalNumber && strconv.Itoa(number) == signal
	}
	name := strings.TrimPrefix(signal, "SIG")
	if _, ok := stopSignalNames[name]; ok {
		return true
	}
	return validRealtimeStopSignal(name)
}

// validRealtimeStopSignal accepts the realtime spellings Docker's signal
// parser maps: RTMIN with offsets 0..15 and RTMAX with offsets 0..14 fill
// the Linux realtime signal range.
func validRealtimeStopSignal(name string) bool {
	switch name {
	case "RTMIN", "RTMAX":
		return true
	}
	if offset, ok := strings.CutPrefix(name, "RTMIN+"); ok {
		return stopSignalOffset(offset, 15)
	}
	if offset, ok := strings.CutPrefix(name, "RTMAX-"); ok {
		return stopSignalOffset(offset, 14)
	}
	return false
}

// stopSignalOffset returns whether offset is a canonical decimal integer in
// 1..max. A zero offset is rejected: Docker's signal parser accepts SIGRTMIN
// and SIGRTMAX on their own but rejects the explicit "+0" and "-0" spellings,
// so accepting them here would only move the failure to the CLI.
func stopSignalOffset(offset string, max int) bool {
	number, err := strconv.Atoi(offset)
	return err == nil && number >= 1 && number <= max && strconv.Itoa(number) == offset
}

// WithStopSignal sets the signal sent to the container's main process when it
// is stopped (Docker's --stop-signal). The value must be a positive signal
// number in Linux's 1..64 range, a standard signal name such as SIGTERM or
// TERM, or a realtime spelling such as SIGRTMIN+3.
// Apple Container has no run-time stop-signal flag and rejects it with
// ErrUnsupportedCapability before invoking its CLI.
func WithStopSignal(signal string) Option {
	return func(c *config) error {
		if !validStopSignal(signal) {
			// Do not echo the rejected value: it may contain control
			// characters or a flag-shaped spelling.
			return newValidationErrorWithField("WithStopSignal", "signal", nil,
				fmt.Errorf("invalid stop signal: want a positive number or an uppercase signal name"))
		}
		c.stopSignal = signal
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

// WithWorkingDir sets the working directory of the init process.
func WithWorkingDir(dir string) Option {
	return func(c *config) error {
		if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\r\n\x00") || !utf8.ValidString(dir) {
			return validationErrorf("WithWorkingDir", dir,
				"working directory %q must be an absolute path without control characters or invalid UTF-8", dir)
		}
		c.workdir = dir
		return nil
	}
}
