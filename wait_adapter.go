package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// WithWaitStrategy blocks Run until the strategy reports the container
// ready. On failure, a non-reuse Run attempts to remove the container
// and, when available, attaches a bounded tail of its logs; a reuse wait
// leaves the shared container in place.
func WithWaitStrategy(s wait.Strategy) Option {
	return func(c *config) error {
		if err := wait.Validate(s); err != nil {
			return err
		}
		c.waitStrategy = s
		c.waitDiagnosticSecrets = wait.DiagnosticValues(s)
		return nil
	}
}

// waitTarget adapts *Container to wait.Target.
type waitTarget struct {
	c *Container
}

var (
	_ wait.Target           = waitTarget{}
	_ wait.StateTarget      = waitTarget{}
	_ wait.PortListenTarget = waitTarget{}
	_ wait.HealthTarget     = waitTarget{}
	_ wait.ExecOutputTarget = waitTarget{}
)

// selectedPort resolves an empty port request the same way for Endpoint and
// the container-side listen check. Port probes are TCP-only: a UDP
// declaration must not silently become the target of an implicit
// ForExposedPort/ForHTTP probe. Exposed declarations retain their order even
// when another port has an explicit host binding; published-only containers
// fall back to their first TCP binding.
func (t waitTarget) selectedPort(port string) (string, error) {
	if port != "" {
		return port, nil
	}
	for _, exposed := range t.c.exposed {
		if exposed.Proto == "tcp" {
			return exposed.String(), nil
		}
	}
	for _, published := range t.c.published {
		if published.Proto == "tcp" {
			return strconv.Itoa(published.ContainerPort) + "/" + published.Proto, nil
		}
	}
	return "", ErrPortNotExposed
}

func (t waitTarget) Endpoint(ctx context.Context, port string) (string, error) {
	resolved, err := t.selectedPort(port)
	if err != nil {
		return "", err
	}
	endpoint, err := t.c.Endpoint(ctx, resolved)
	return endpoint, waitTargetError(err)
}

// procNetExecTimeout bounds one container-side listen check so a hung exec
// cannot consume the whole startup timeout. The wait context still wins when
// it expires first.
const procNetExecTimeout = 5 * time.Second

// procNetMaxLineBytes bounds one retained /proc/net line. Socket rows are
// well under 256 bytes; the cap only keeps a malformed stream from growing
// the scanner's line buffer without bound.
const procNetMaxLineBytes = 4096

// procNetCommand reads both address families without a shell or an external
// port tool. Images without cat (for example distroless) make the check
// report unsupported and fall back to the host connection.
func procNetCommand() []string {
	return []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"}
}

// ContainerPortStatus reads the container's /proc/net tables and reports
// whether the declared port is in LISTEN state. Docker's userland proxy
// accepts a host connection before the container process listens, so a
// successful host dial alone is not proof of readiness. Apple Container
// connects directly to the VM endpoint and needs no extra check.
func (t waitTarget) ContainerPortStatus(ctx context.Context, port string) (wait.PortListenStatus, error) {
	if t.c.eng.directIP() {
		return wait.PortListenUnsupported, nil
	}
	resolved, err := t.selectedPort(port)
	if err != nil {
		return wait.PortListenUnsupported, err
	}
	spec, err := parsePortSpec(resolved)
	if err != nil {
		return wait.PortListenUnsupported, err
	}
	// Stream the table into a line scanner instead of capturing a bounded
	// prefix: a busy container's /proc/net/tcp can exceed any fixed capture
	// limit, and that used to truncate /proc/net/tcp6 entirely and hide an
	// IPv6-only listener.
	scanner := newProcNetScanner(spec.Port)
	code, _, err := t.c.ExecTo(ctx, procNetCommand(), scanner,
		WithExecTimeout(procNetExecTimeout),
		// An image without /proc/net/tcp6 writes cat's diagnostics to
		// stderr. Keep them out of the table the scanner parses.
		WithExecStderr(io.Discard),
	)
	if err != nil {
		if portCheckUnavailable(err) {
			return wait.PortListenUnsupported, nil
		}
		// Container-side probing failures are retryable; the wait loop
		// decides when the startup timeout has expired.
		return wait.PortListenUnsupported, waitTargetError(err)
	}
	// 126 and 127 mean the command could not be invoked, which is how a
	// distroless image without cat surfaces. Do not let a partial table
	// from such a wrapper hide the unavailable check.
	if code == 126 || code == 127 {
		return wait.PortListenUnsupported, nil
	}
	if !scanner.supported {
		return wait.PortListenUnsupported, nil
	}
	if scanner.listening {
		return wait.PortListenOpen, nil
	}
	if scanner.loopbackListening && t.hostNetwork() {
		// A host-network container shares the host loopback namespace, so a
		// loopback-only listener is reachable through the resolved endpoint.
		return wait.PortListenOpen, nil
	}
	return wait.PortListenClosed, nil
}

// hostNetwork reports whether the container shares the host's network
// namespace. Only there is a listener bound to 127.0.0.0/8 or ::1 reachable
// from the host endpoint; across a bridge network it is not published.
func (t waitTarget) hostNetwork() bool {
	if t.c.networkExplicit && t.c.network == dockerNetworkHost {
		return true
	}
	// Reuse and daemon-default containers describe their mode only through
	// inspect. Endpoint resolution runs before this check and populates the
	// short-TTL snapshot, so reading it does not add a backend call.
	if info := t.c.cachedDynamic(); info != nil {
		return info.networkMode == dockerNetworkHost
	}
	return false
}

// portCheckUnavailable reports whether an exec failure means the
// container-side check cannot run at all, rather than a transient failure
// worth retrying. Context and backend availability errors stay retryable.
func portCheckUnavailable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrUnsupportedCapability) {
		return true
	}
	var launchErr *exec.Error
	return errors.As(err, &launchErr)
}

// procNetScanner recognizes LISTEN sockets while ExecTo streams the
// container's /proc/net/tcp and /proc/net/tcp6 tables. It keeps only the
// current line and stops classifying after the first match, so a large
// table neither needs a full-output buffer nor allocates a string and a
// field slice per line.
type procNetScanner struct {
	port int
	line []byte
	// listening reports an externally reachable LISTEN socket. A listener
	// confined to 127.0.0.0/8 or ::1 only counts on a host-network
	// container, so it is tracked separately.
	listening         bool
	loopbackListening bool
	supported         bool
	skipLine          bool
}

func newProcNetScanner(port int) *procNetScanner {
	return &procNetScanner{port: port, line: make([]byte, 0, 256)}
}

// Write consumes one output chunk. Rows are classified only when their
// trailing newline arrives: /proc/net/tcp always terminates rows, and a
// truncated transfer must not invent a partial final row. Bytes after a
// found listener are drained without further parsing; the writer accepts
// everything so the exec child is never blocked on a full pipe.
func (s *procNetScanner) Write(p []byte) (int, error) {
	original := len(p)
	for len(p) > 0 && !s.listening {
		newline := bytes.IndexByte(p, '\n')
		if newline < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:newline])
		s.classifyLine()
		p = p[newline+1:]
	}
	return original, nil
}

func (s *procNetScanner) appendLine(fragment []byte) {
	if s.skipLine || len(fragment) == 0 {
		return
	}
	if len(s.line)+len(fragment) > procNetMaxLineBytes {
		s.skipLine = true
		s.line = s.line[:0]
		return
	}
	s.line = append(s.line, fragment...)
}

func (s *procNetScanner) classifyLine() {
	if !s.skipLine {
		listening, loopback, socketLine, headerLine := scanProcNetLine(s.line, s.port)
		if listening {
			s.listening = true
		}
		if loopback {
			s.loopbackListening = true
		}
		if socketLine || headerLine {
			s.supported = true
		}
	}
	s.line = s.line[:0]
	s.skipLine = false
}

var (
	procNetHeaderFirstField  = []byte("sl")
	procNetHeaderSecondField = []byte("local_address")
	procNetListenStateField  = []byte("0A")
)

// scanProcNetLine classifies one table line without allocating. socketLine
// reports a parseable socket row, headerLine reports the table header, and
// listening requires a socket row for port in LISTEN state bound to an
// address reachable from outside the container. loopback reports the same
// LISTEN row bound only to the container's loopback interface.
func scanProcNetLine(line []byte, port int) (listening, loopback, socketLine, headerLine bool) {
	var fields [4][]byte
	count := 0
	for index := 0; index < len(line) && count < len(fields); {
		for index < len(line) && isProcNetSpace(line[index]) {
			index++
		}
		if index >= len(line) {
			break
		}
		start := index
		for index < len(line) && !isProcNetSpace(line[index]) {
			index++
		}
		fields[count] = line[start:index]
		count++
	}
	if count < len(fields) {
		return false, false, false, false
	}
	if bytes.Equal(fields[0], procNetHeaderFirstField) && bytes.Equal(fields[1], procNetHeaderSecondField) {
		return false, false, false, true
	}
	localPort, ok := procNetLocalPort(fields[1])
	if !ok {
		return false, false, false, false
	}
	if localPort != port || !bytes.Equal(fields[3], procNetListenStateField) {
		return false, false, true, false
	}
	isLoopback, ok := procNetLoopbackAddress(fields[1])
	if ok && isLoopback {
		return false, true, true, false
	}
	return true, false, true, false
}

// procNetLoopbackAddress decodes the local_address field and reports whether
// it is a loopback address. The kernel prints each 32-bit word in host byte
// order, so the bytes within each word are reversed for both address
// families.
func procNetLoopbackAddress(field []byte) (bool, bool) {
	colon := bytes.LastIndexByte(field, ':')
	if colon < 0 {
		return false, false
	}
	address := field[:colon]
	switch len(address) {
	case 8:
		var octets [4]byte
		for i := range octets {
			value, ok := procNetHexByte(address[i*2 : i*2+2])
			if !ok {
				return false, false
			}
			octets[len(octets)-1-i] = value
		}
		return octets[0] == 127, true
	case 32:
		var parsed [16]byte
		for word := 0; word < 4; word++ {
			for i := 0; i < 4; i++ {
				value, ok := procNetHexByte(address[word*8+i*2 : word*8+i*2+2])
				if !ok {
					return false, false
				}
				parsed[word*4+(3-i)] = value
			}
		}
		return procNetIPv6Loopback(parsed), true
	default:
		return false, false
	}
}

func procNetHexByte(pair []byte) (byte, bool) {
	high, ok := procNetHexDigit(pair[0])
	if !ok {
		return 0, false
	}
	low, ok := procNetHexDigit(pair[1])
	if !ok {
		return 0, false
	}
	return byte(high<<4 | low), true
}

func procNetIPv6Loopback(address [16]byte) bool {
	// ::1
	if procNetZeroBytes(address[:15]) && address[15] == 1 {
		return true
	}
	if address[12] != 127 {
		return false
	}
	// IPv4-compatible ::127.0.0.0/8 and IPv4-mapped ::ffff:127.0.0.0/8.
	if procNetZeroBytes(address[:12]) {
		return true
	}
	return procNetZeroBytes(address[:10]) && address[10] == 0xff && address[11] == 0xff
}

func procNetZeroBytes(values []byte) bool {
	for _, value := range values {
		if value != 0 {
			return false
		}
	}
	return true
}

func procNetLocalPort(field []byte) (int, bool) {
	colon := bytes.LastIndexByte(field, ':')
	if colon < 0 {
		return 0, false
	}
	digits := field[colon+1:]
	if len(digits) == 0 || len(digits) > 4 {
		return 0, false
	}
	port := 0
	for _, digit := range digits {
		value, ok := procNetHexDigit(digit)
		if !ok {
			return 0, false
		}
		port = port<<4 | value
	}
	return port, true
}

func procNetHexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

func isProcNetSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\v', '\f', '\r':
		return true
	}
	return false
}

func (t waitTarget) Running(ctx context.Context) (bool, error) {
	state, err := t.State(ctx)
	return state == wait.StateRunning, err
}

func (t waitTarget) State(ctx context.Context) (wait.State, error) {
	if t.c == nil || t.c.eng == nil {
		return wait.StateRunning, nil
	}
	state, err := t.c.State(ctx)
	if err != nil {
		return wait.StateUnknown, waitTargetError(err)
	}
	// wait.State aliases domain.State, so the adapter can report the
	// container's state without a parallel enumeration. wait canonicalizes
	// unrecognized values before acting on them.
	return state, nil
}

func (t waitTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	stream, err := t.c.FollowLogs(ctx)
	return stream, waitTargetError(err)
}

func (t waitTarget) Health(ctx context.Context) (wait.Health, error) {
	if err := ctx.Err(); err != nil {
		return wait.HealthNone, err
	}
	if t.c == nil || t.c.eng == nil {
		return wait.HealthNone, nil
	}
	if !t.c.eng.capabilities().healthcheck {
		return wait.HealthNone, fmt.Errorf("%w: %w", ErrUnsupportedCapability, &wait.ConfigError{
			Strategy: "ForHealthcheck",
			Field:    "backend",
			Value:    t.c.eng.name(),
			Reason:   "backend does not report Docker HEALTHCHECK status",
		})
	}
	health, err := t.c.healthStatus(ctx)
	if err != nil {
		return wait.HealthNone, waitTargetError(err)
	}
	return wait.Health(health), nil
}

func (t waitTarget) ExecCommand(ctx context.Context, cmd []string) (int, error) {
	// Readiness probes discard application output. ExecTo drains the CLI
	// streams into io.Discard instead of materializing a buffer for every
	// polling attempt.
	code, _, err := t.c.ExecTo(ctx, cmd, io.Discard)
	return code, waitTargetError(err)
}

// waitExecOutputLimit bounds the command output a wait matcher can inspect.
// Readiness markers are short, so a command that prints unboundedly must not
// turn a poll into a memory cost.
const waitExecOutputLimit = 64 * 1024

// ExecCommandOutput returns the command's combined stdout and stderr, capped
// at waitExecOutputLimit bytes, for output-matching readiness probes.
func (t waitTarget) ExecCommandOutput(ctx context.Context, cmd []string) (int, []byte, error) {
	var output bytes.Buffer
	code, _, err := t.c.ExecTo(ctx, cmd, &output, WithExecMaxBytes(waitExecOutputLimit))
	return code, output.Bytes(), waitTargetError(err)
}

func waitTargetError(err error) error {
	if err == nil {
		return nil
	}
	// A rejected public option cannot be repaired by polling. Surface it as
	// a configuration failure so ForExec-style retries stop immediately
	// while the original *ValidationError stays in the chain.
	if errors.Is(err, ErrInvalidOption) && !errors.Is(err, wait.ErrInvalidConfiguration) {
		return fmt.Errorf("%w: %w", wait.ErrInvalidConfiguration, err)
	}
	if !isNotFound(err) || errors.Is(err, wait.ErrTargetNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", wait.ErrTargetNotFound, err)
}

// logTailLimit bounds the diagnostic log tail attached to wait
// failures.
const logTailLimit = 1024 * 1024

// logTailWithError fetches up to logTailLimit trailing bytes of the
// verified operation target's logs for diagnostics. It asks the backend
// for a bounded tail (logsTailArgs) and keeps only the last bytes in a
// fixed-size ring, so neither the CLI output nor the Go buffer grows with
// total log size. Resolving the target first keeps a deleted or replaced
// same-name container from contributing unrelated logs to the error.
func (c *Container) logTailWithError(ctx context.Context, extra ...string) (string, error) {
	qCtx, cancel := context.WithTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(qCtx)
	if err != nil {
		return "", err
	}
	return c.logTailTarget(qCtx, target, extra...)
}

func (c *Container) logTailTarget(ctx context.Context, target string, extra ...string) (string, error) {
	redactor := c.diagnosticRedactor(extra...)
	if streamer, ok := c.runner.(cli.SnapshotStreamer); ok {
		stream, err := streamer.StreamSnapshot(ctx, c.eng.logsTailArgs(target)...)
		if err != nil {
			return "", err
		}
		defer stream.Close()
		return redactor.RedactTail(stream, logTailLimit)
	}
	stdout, stderr, err := c.runner.Run(ctx, c.eng.logsTailArgs(target)...)
	if err != nil {
		return "", err
	}
	return redactor.RedactTail(
		io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)),
		logTailLimit,
	)
}
