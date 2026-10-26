package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
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
		if exposed.proto == "tcp" {
			return exposed.String(), nil
		}
	}
	for _, published := range t.c.published {
		if published.proto == "tcp" {
			return strconv.Itoa(published.containerPort) + "/" + published.proto, nil
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

// procNetReadLimit bounds the bytes retained from /proc/net; the exec sink
// drains anything beyond it.
const procNetReadLimit = 1 << 20

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
	var out bytes.Buffer
	code, _, err := t.c.ExecTo(ctx, procNetCommand(), &out,
		WithExecMaxBytes(procNetReadLimit),
		WithExecTimeout(procNetExecTimeout),
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
	listening, supported := procNetListening(out.Bytes(), spec.port)
	if !supported {
		return wait.PortListenUnsupported, nil
	}
	if listening {
		return wait.PortListenOpen, nil
	}
	return wait.PortListenClosed, nil
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

// procNetListening parses /proc/net/tcp and /proc/net/tcp6 output. supported
// is false when the output contains neither a header nor a socket line,
// which is how a missing cat or an unavailable /proc/net surfaces.
func procNetListening(data []byte, port int) (listening, supported bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[0] == "sl" && fields[1] == "local_address" {
			supported = true
			continue
		}
		colon := strings.LastIndexByte(fields[1], ':')
		if colon < 0 {
			continue
		}
		localPort, err := strconv.ParseUint(fields[1][colon+1:], 16, 16)
		if err != nil {
			continue
		}
		supported = true
		if int(localPort) == port && fields[3] == "0A" {
			listening = true
		}
	}
	return listening, supported
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
	switch state {
	case StateRunning:
		return wait.StateRunning, nil
	case StateStopped:
		return wait.StateStopped, nil
	case StateStopping:
		return wait.StateStopping, nil
	case StateCreated:
		return wait.StateCreated, nil
	case StateRestarting:
		return wait.StateRestarting, nil
	case StatePaused:
		return wait.StatePaused, nil
	default:
		return wait.StateUnknown, nil
	}
}

func (t waitTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	stream, err := t.c.FollowLogs(ctx)
	return stream, waitTargetError(err)
}

func (t waitTarget) ExecCommand(ctx context.Context, cmd []string) (int, error) {
	// Readiness probes discard application output. ExecTo drains the CLI
	// streams into io.Discard instead of materializing a buffer for every
	// polling attempt.
	code, _, err := t.c.ExecTo(ctx, cmd, io.Discard)
	return code, waitTargetError(err)
}

func waitTargetError(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, wait.ErrTargetNotFound) {
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
	qCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
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
