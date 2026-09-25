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

// maxStderr bounds the stderr captured into a CLIError.
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
	Stderr   string
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
		return stdout.Bytes(), stderr.Bytes(), commandRunError(ctx, bin, args, stderr.Bytes(), err)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func commandRunError(ctx context.Context, bin string, args []string, stderr []byte, err error) error {
	// A canceled CommandContext commonly returns *exec.ExitError after
	// killing the process. Preserve the command result before consulting
	// ctx.Err, then join both causes when cancellation raced the exit.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		cliErr := &CLIError{
			Binary:   bin,
			Args:     args,
			ExitCode: exitErr.ExitCode(),
			Stderr:   truncateStderr(string(stderr)),
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			contextErr := fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr)
			return errors.Join(cliErr, contextErr)
		}
		return cliErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr)
	}
	return err
}

// truncateStderr bounds the diagnostic copy kept in CLIError.
func truncateStderr(s string) string {
	if len(s) > maxStderr {
		return s[:maxStderr]
	}
	return s
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller already gave up. Keep both the original failure and
		// the context cause so cancellation remains observable.
		if err == nil {
			return ctxErr
		}
		if errors.Is(err, ctxErr) {
			return err
		}
		return errors.Join(err, ctxErr)
	}
	if err == nil {
		return nil
	}
	if isContextError(err) {
		return err
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if stdout, stderr, probeErr := r.Run(probeCtx, probe.Args...); probeErr != nil {
		probeErr = withProbeOutput(probeErr, stdout, stderr)
		// A caller cancellation must not be relabeled as a stopped
		// backend. Keep the probe result and context as diagnostic context.
		if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
			return classified
		}
		// A permission/configuration failure in the original command is
		// not evidence that the backend is down when only the conservative
		// default matcher is available. An explicit backend predicate is
		// authoritative for broad original text, but definite permission,
		// cancellation, launch, and endpoint-configuration failures still
		// veto the sentinel.
		if isNonLivenessError(err) &&
			(probe.IsUnavailable == nil || isDefinitiveNonLivenessError(err)) {
			if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
				return classified
			}
			return errors.Join(err, probeErr)
		}
		// A returned timeout is liveness evidence when the only additional
		// cause is the command exit produced by the probe itself. A joined
		// permission, cancellation, or configuration failure must veto it.
		if errors.Is(probeErr, context.DeadlineExceeded) {
			if !isDefinitiveProbeTimeout(probeErr) {
				if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
					return classified
				}
				return errors.Join(err, probeErr)
			}
			if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
				return classified
			}
			return errors.Join(
				fmt.Errorf("%w: %s", ErrSystemNotRunning, probe.Hint),
				err,
				probeErr,
			)
		}
		// A direct cancellation or a permission/configuration failure is
		// not proof that the backend is down. This check includes every
		// joined/wrapped cause.
		if isNonLivenessError(probeErr) {
			if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
				return classified
			}
			return errors.Join(err, probeErr)
		}
		// Recheck immediately before invoking backend code: predicates may
		// perform instrumentation and cancellation can race their work.
		if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
			return classified
		}
		unavailable := probeUnavailable(probe, probeErr)
		if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
			return classified
		}
		if unavailable {
			// Recheck once more at the sentinel boundary. Predicate work and
			// result construction must never mask a caller cancellation.
			if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
				return classified
			}
			return errors.Join(
				fmt.Errorf("%w: %s", ErrSystemNotRunning, probe.Hint),
				err,
				probeErr,
			)
		}
		// A failed probe is not proof that the backend is down. Keep both
		// errors so callers can inspect the original command and probe
		// diagnostics without losing either chain.
		if classified, canceled := joinCallerCancellation(ctx, err, probeErr); canceled {
			return classified
		}
		return errors.Join(err, probeErr)
	}
	if classified, canceled := joinCallerCancellation(ctx, err, nil); canceled {
		return classified
	}
	return err
}

func joinCallerCancellation(ctx context.Context, original, probeErr error) (error, bool) {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return nil, false
	}
	return errors.Join(original, probeErr, ctxErr), true
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type probeOutputError struct {
	err    error
	output string
}

func (e *probeOutputError) Error() string {
	return e.err.Error() + "\n" + e.output
}

func (e *probeOutputError) Unwrap() error { return e.err }

// withProbeOutput makes successful probe output available to the
// backend-specific liveness predicates. Some CLIs report status on stdout
// while still exiting non-zero. The original error remains the unwrap target,
// so wrapping output does not discard the probe chain.
func withProbeOutput(err error, stdout, stderr []byte) error {
	diagnostic := err.Error()
	var output []string
	for _, stream := range [][]byte{stdout, stderr} {
		text := strings.TrimSpace(string(truncateStderr(string(stream))))
		if text != "" && !strings.Contains(diagnostic, text) {
			output = append(output, text)
		}
	}
	if len(output) == 0 {
		return err
	}
	return &probeOutputError{err: err, output: strings.Join(output, "\n")}
}

func probeUnavailable(probe Probe, err error) bool {
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
	// Error includes wrapper and joined-branch text outside the first
	// CLIError.Stderr. Requiring a reachable CLIError keeps arbitrary
	// application errors from becoming liveness evidence on their own.
	return defaultProbeUnavailableText(strings.ToLower(err.Error()))
}

func isDefinitiveProbeTimeout(err error) bool {
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if IsProbeConfigurationError(err) || containsDefinitiveNonLivenessText(err.Error()) {
		return false
	}
	return errorTreeHasOnlyTimeoutEvidence(err)
}

// errorTreeHasOnlyTimeoutEvidence accepts the shape produced when an
// ExecRunner's child exits as the probe deadline fires: a command CLIError
// joined with context.DeadlineExceeded. Arbitrary joined failures still veto
// liveness, as do CLI launch/permission exits and configuration diagnostics.
func errorTreeHasOnlyTimeoutEvidence(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !errorTreeHasOnlyTimeoutEvidence(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return errorTreeHasOnlyTimeoutEvidence(cause)
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		return cliErr.ExitCode != 126 && cliErr.ExitCode != 127
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func defaultProbeUnavailableText(s string) bool {
	for _, fragment := range []string{
		"cannot connect",
		"connection refused",
		"xpc connection",
		"system is not running",
		"daemon is not running",
		"is the docker daemon running",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// IsProbeConfigurationError reports concrete transport, authentication,
// and endpoint-configuration diagnostics that must not be classified as a
// stopped backend. The complete wrapped/joined error text is inspected so
// instrumentation cannot hide a configuration failure in another branch.
func IsProbeConfigurationError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"tls",
		"x509",
		"certificate",
		"ssh",
		"proxy",
		"config",
		"authentication required",
		"authentication failed",
		"unauthorized",
		"forbidden",
		"credential",
		"invalid context",
		"unknown context",
		"no such context",
		"context not found",
		"context does not exist",
		"unknown flag",
		"unknown option",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// isDefinitiveNonLivenessError identifies original failures that remain a
// veto even when an explicit backend liveness predicate is available. Broad
// words such as "configuration" are intentionally absent.
func isDefinitiveNonLivenessError(err error) bool {
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
	return containsDefinitiveNonLivenessText(err.Error())
}

func containsDefinitiveNonLivenessText(text string) bool {
	s := strings.ToLower(text)
	for _, fragment := range []string{
		"permission denied",
		"operation not permitted",
		"operation canceled",
		"operation cancelled",
		"context canceled",
		"context cancelled",
		"eacces",
		"eperm",
		"access denied",
		"not permitted",
		"invalid config",
		"config error",
		"invalid configuration",
		"invalid context",
		"unknown context",
		"no such context",
		"context not found",
		"context does not exist",
		"unknown flag",
		"unknown option",
		"tls",
		"x509",
		"certificate",
		"ssh",
		"proxy",
		"authentication required",
		"authentication failed",
		"unauthorized",
		"forbidden",
		"credential",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// isNonLivenessError identifies failures that must not be relabeled as a
// stopped backend, even when the probe happens to fail too.
func isNonLivenessError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	if IsProbeConfigurationError(err) {
		return true
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return containsNonLivenessText(strings.ToLower(err.Error()))
	}
	if cliErr.ExitCode == 126 || cliErr.ExitCode == 127 {
		return true
	}
	// Wrappers and errors.Join branches can carry additional diagnostics
	// outside the first CLIError.Stderr.
	return containsNonLivenessText(strings.ToLower(err.Error()))
}

// containsNonLivenessText also treats object/image absence diagnostics as
// non-liveness evidence. A failed probe cannot turn an ambiguous missing
// object message into proof that the daemon stopped.
func containsNonLivenessText(s string) bool {
	for _, fragment := range []string{
		"permission denied",
		"operation not permitted",
		"operation canceled",
		"operation cancelled",
		"eacces",
		"eperm",
		"access denied",
		"not permitted",
		"configuration",
		"invalid config",
		"config error",
		"invalid context",
		"unknown context",
		"no such context",
		"container not found",
		"image not found",
		"no such container",
		"no such object",
		"no such image",
		"context not found",
		"context does not exist",
		"context canceled",
		"context cancelled",
		"deadline exceeded",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}
