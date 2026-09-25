// Package cli executes a container backend CLI (`container` or `docker`)
// as a child process and classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxStderr bounds each diagnostic stream copied into a CLIError.
const maxStderr = 64 * 1024

// ErrSystemNotRunning reports that the container backend (Apple
// Container system service or Docker daemon) is not running.
var ErrSystemNotRunning = errors.New("container backend is not running")

// Probe is the backend-specific liveness check Classify runs after a
// failure: a cheap CLI invocation plus the hint to show the user when
// it fails.
type Probe struct {
	Args []string
	Hint string
	// IsUnavailable reports whether a probe failure means that the
	// backend is not running. A nil predicate uses conservative
	// liveness-text matching; caller cancellation and other
	// non-liveness errors are rejected before this predicate is used.
	IsUnavailable func(error) bool
}

// Runner executes one backend CLI invocation.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}

// ExternalRunner identifies runners that execute the CLI as real child
// processes. container.Run registers containers started through such
// runners with the orphan-cleanup reaper, so a runner that wraps an
// ExecRunner forwards both methods to keep the production path intact
// under instrumentation. Test doubles that return canned results do
// not implement the interface.
type ExternalRunner interface {
	Runner

	// External reports whether the runner spawns real child processes.
	External() bool
	// ExternalBinary is the binary those child processes execute, or
	// "" when the runner defers to the engine's default.
	ExternalBinary() string
}

// External reports that ExecRunner spawns real child processes.
func (r *ExecRunner) External() bool { return true }

// ExternalBinary reports the binary ExecRunner spawns.
func (r *ExecRunner) ExternalBinary() string { return r.Binary }

// CLIError is a non-zero exit from a backend CLI.
type CLIError struct {
	// Binary is the CLI executable that failed (e.g. "container" or
	// "docker"). Empty means the historical default of "container".
	Binary   string
	Args     []string
	ExitCode int
	// Stdout and Stderr retain bounded diagnostic copies from a failed
	// invocation. Raw output remains available from Runner.Run.
	Stdout string
	Stderr string
}

func (e *CLIError) Error() string {
	bin := e.Binary
	if bin == "" {
		bin = "container"
	}
	msg := fmt.Sprintf("%s %s: exit code %d", bin, strings.Join(e.Args, " "), e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + strings.TrimSpace(e.Stderr)
	}
	if e.Stdout != "" {
		if e.Stderr != "" {
			msg += "; stdout: "
		} else {
			msg += ": "
		}
		msg += strings.TrimSpace(e.Stdout)
	}
	return msg
}

// ExecRunner runs the CLI as a child process. Arguments are passed as an
// argv vector; no shell is involved.
type ExecRunner struct {
	// Binary is the CLI executable. Empty means "container" resolved
	// from PATH.
	Binary string
}

func (r *ExecRunner) binary() string {
	if r.Binary == "" {
		return "container"
	}
	return r.Binary
}

func (r *ExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// If the process ignores the kill long enough to hold pipes open,
	// give up waiting shortly after.
	cmd.WaitDelay = 3 * time.Second

	err := cmd.Run()
	// Output buffers are returned whole: success output and non-zero
	// exec/log results must not be silently truncated. Only the
	// diagnostic copy inside CLIError is bounded.
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			cliErr := &CLIError{
				Binary:   bin,
				Args:     args,
				ExitCode: exitErr.ExitCode(),
				Stdout:   truncateOutput(stdout.String()),
				Stderr:   truncateStderr(stderr.String()),
			}
			// Cancellation can race with observing a real command exit.
			// Preserve both facts so classification can still inspect the
			// CLIError after Run returns.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stdout.Bytes(), stderr.Bytes(), errors.Join(cliErr, ctxErr)
			}
			return stdout.Bytes(), stderr.Bytes(), cliErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr)
		}
		return stdout.Bytes(), stderr.Bytes(), err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// truncateOutput bounds a diagnostic copy kept in CLIError.
func truncateOutput(s string) string {
	if len(s) > maxStderr {
		return s[:maxStderr]
	}
	return s
}

// truncateStderr bounds the stderr diagnostic copy kept in CLIError.
func truncateStderr(s string) string {
	return truncateOutput(s)
}

// IsCommandExit reports whether err is a CLIError from a child process
// that started and returned an exit status. Launch failures (missing
// binary, OS exec errors) are not CLIErrors and return false.
func IsCommandExit(err error) bool {
	var e *CLIError
	return errors.As(err, &e)
}

// probeTimeout bounds the diagnostic liveness check so a hung backend
// cannot stall error handling forever. Caller cancellation still
// aborts the probe via context propagation.
const probeTimeout = 5 * time.Second

// Classify augments a failed CLI call. When the backend-specific probe
// identifies a liveness failure, the result is marked with
// ErrSystemNotRunning. The original and probe errors remain in the
// result's error chain in every probe-failure case.
func Classify(ctx context.Context, r Runner, err error, probe Probe) error {
	if err == nil {
		return nil
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// No probe is useful after the caller has given up. Keep the
		// cancellation in the chain so callers can still distinguish it
		// from the original command failure.
		return errors.Join(err, ctxErr)
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, _, probeErr := r.Run(probeCtx, probe.Args...)
	if probeErr == nil {
		// A runner can return successfully just as the caller cancels.
		// Do not lose that cancellation, but never invent a liveness
		// failure for it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(err, ctxErr)
		}
		return err
	}

	// Cancellation takes precedence over every probe classification. It
	// may race with the runner returning, so check it before inspecting
	// the diagnostic and retain caller and probe context errors explicitly
	// when the runner did not wrap them.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	if isNonLivenessError(err) {
		// The command already identified a configuration, permission,
		// TLS, or other client-side failure. A failed probe cannot
		// replace that diagnosis with daemon-down.
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}

	// A timeout belonging to the bounded probe is evidence that the
	// backend did not answer, unless the runner reported a distinct
	// non-liveness error. A caller deadline was handled above.
	if isProbeNonLiveness(probeCtx, probeErr) {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	if probeCtx.Err() == context.DeadlineExceeded {
		return classifySystemNotRunning(ctx, probeCtx, err, probeErr, probe.Hint)
	}

	// Check immediately before invoking a backend predicate. The
	// predicate may be user-provided and can itself take time.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	unavailable := probeUnavailable(probe, probeErr)
	// Check again after the predicate. In particular, a predicate that
	// cancels the caller and then reports liveness must not win the
	// race and add ErrSystemNotRunning.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	if unavailable {
		return classifySystemNotRunning(ctx, probeCtx, err, probeErr, probe.Hint)
	}

	// A failed probe is not proof that the backend is down. Keep both
	// errors so callers can inspect the original diagnostic.
	return joinProbeFailure(ctx, probeCtx, err, probeErr)
}

func joinProbeFailure(ctx, probeCtx context.Context, original, probeErr error) error {
	joined := withContextError(probeCtx, errors.Join(original, probeErr))
	return withContextError(ctx, joined)
}

func withContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return errors.Join(err, ctxErr)
	}
	return err
}

func classifySystemNotRunning(ctx, probeCtx context.Context, original, probeErr error, hint string) error {
	// Keep this check at the point where the sentinel is added. Context
	// cancellation can race with all of the preceding diagnostics checks.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, original, probeErr)
	}
	classified := errors.Join(
		fmt.Errorf("%w: %s", ErrSystemNotRunning, hint),
		withContextError(probeCtx, errors.Join(original, probeErr)),
	)
	// Building the joined error can itself race with cancellation. Do
	// not return a daemon-down result if cancellation was observed while
	// it was being assembled.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, original, probeErr)
	}
	return classified
}

func isProbeNonLiveness(probeCtx context.Context, err error) bool {
	if !isNonLivenessError(err) {
		return false
	}
	// A context deadline returned by the bounded probe is the timeout
	// signal itself, not a reason to suppress the liveness sentinel.
	// Keep a simultaneous client-side diagnostic non-liveness, though.
	if probeCtx.Err() == context.DeadlineExceeded && errors.Is(err, context.DeadlineExceeded) {
		var cliErr *CLIError
		if errors.As(err, &cliErr) && (cliErr.ExitCode == 126 || cliErr.ExitCode == 127) {
			return true
		}
		return hasNonContextNonLivenessText(err)
	}
	return true
}

func hasNonContextNonLivenessText(err error) bool {
	s := strings.ToLower(cliDiagnosticText(err))
	for _, contextFragment := range []string{
		"context deadline exceeded",
		"deadline exceeded",
		"context canceled",
		"context cancelled",
	} {
		s = strings.ReplaceAll(s, contextFragment, "")
	}
	return containsNonLivenessText(s)
}

func probeUnavailable(probe Probe, err error) bool {
	if isNonLivenessError(err) {
		return false
	}
	if probe.IsUnavailable != nil {
		return probe.IsUnavailable(err)
	}
	return defaultProbeUnavailable(err)
}

func defaultProbeUnavailable(err error) bool {
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	s := strings.ToLower(cliDiagnosticText(err))
	for _, fragment := range []string{
		"cannot connect",
		"connection refused",
		"xpc connection",
		"system is not running",
		"apiserver is not running",
		"not registered with launchd",
		"daemon is not running",
		"is the docker daemon running",
		"error during connect",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func cliDiagnosticText(err error) string {
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		return strings.Join([]string{cliErr.Stdout, cliErr.Stderr}, "\n")
	}
	return err.Error()
}

// IsNonLivenessError reports whether err identifies a client-side
// failure that must not be relabeled as a stopped backend. It is used
// by backend-specific probe predicates as well as Classify.
func IsNonLivenessError(err error) bool {
	return isNonLivenessError(err)
}

// isNonLivenessError identifies failures that must not be relabeled
// as a stopped backend, even when the probe happens to fail too.
func isNonLivenessError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) && (cliErr.ExitCode == 126 || cliErr.ExitCode == 127) {
		return true
	}
	return containsNonLivenessText(strings.ToLower(cliDiagnosticText(err)))
}

func containsNonLivenessText(s string) bool {
	for _, fragment := range []string{
		// Permission and authorization failures. Keep these as diagnostic
		// phrases: bare "permission" can occur in an endpoint path.
		"permission denied",
		"insufficient permissions",
		"operation not permitted",
		"operation canceled",
		"operation cancelled",
		"eacces",
		"eperm",
		"access denied",
		"not permitted",
		"unauthorized",
		"forbidden",
		// Configuration and context failures. A bare "config" can be a
		// legitimate directory or filename in a backend endpoint.
		"invalid config",
		"invalid configuration",
		"config file",
		"configuration file",
		"config directory",
		"configuration directory",
		"invalid context",
		"unknown context",
		"no such context",
		"context not found",
		"context does not exist",
		"context canceled",
		"context cancelled",
		"deadline exceeded",
		// TLS and certificate verification failures. A bare "certificate"
		// can also be a hostname or endpoint-path component.
		"tls",
		"x509",
		"certificate signed by unknown authority",
		"certificate verification failed",
		"certificate verify failed",
		"certificate has expired",
		"certificate is not trusted",
		"certificate is invalid",
		"unable to verify certificate",
		"failed to verify certificate",
		"unknown authority",
		"handshake failure",
		"http response to https",
		// Credential-helper and registry authentication failures.
		"credential",
		"credentials",
		"authentication failed",
		// Invalid command-line flags/options.
		"unknown flag",
		"unknown shorthand flag",
		"flag needs an argument",
		"flag provided but not defined",
		"invalid flag",
		"unknown option",
		"no such option",
		"invalid option",
		"invalid argument",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}
