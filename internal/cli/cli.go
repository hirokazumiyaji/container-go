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
	"regexp"
	"strings"
	"time"
)

// maxStderr bounds the stderr captured into a CLIError.
const maxStderr = 64 * 1024

var endpointURI = regexp.MustCompile(`(?i)\b(unix|tcp|ssh|npipe|http|https)://[^\s"'<>]+`)

// ErrSystemNotRunning reports that the container backend (Apple
// Container system service or Docker daemon) is not running.
var ErrSystemNotRunning = errors.New("container backend is not running")

// Probe is the backend-specific liveness check Classify runs after a
// failure: a cheap CLI invocation plus the hint to show the user when
// it fails.
type Probe struct {
	Args []string
	Hint string
	// IsUnavailable recognizes a backend-specific liveness failure. A
	// failed probe with no proven liveness signal is not enough to add
	// ErrSystemNotRunning.
	IsUnavailable func(error) bool
}

// Runner executes one backend CLI invocation.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}

// Spawner starts the CLI as a child process and returns immediately, so a
// caller can release a name-addressed lock as soon as the process exists.
// The name is resolved by the backend at exec time, so the lock must still
// be held across the Start call itself.
type Spawner interface {
	Start(ctx context.Context, args ...string) (*StartedCommand, error)
}

// StartedCommand is a child process that has been spawned and is still
// running.
type StartedCommand struct {
	cmd    *exec.Cmd
	bin    string
	args   []string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ctx    context.Context
}

// Wait blocks until the child exits and returns its output and error, using
// the same contract as ExecRunner.Run.
func (s *StartedCommand) Wait() ([]byte, []byte, error) {
	err := s.cmd.Wait()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			cliErr := &CLIError{
				Binary:   s.bin,
				Args:     s.args,
				ExitCode: exitErr.ExitCode(),
				Stderr:   truncateStderr(s.stderr.String()),
			}
			commandErr := withStdout(cliErr, s.stdout.String())
			if ctxErr := s.ctx.Err(); ctxErr != nil {
				return s.stdout.Bytes(), s.stderr.Bytes(), errors.Join(commandErr, ctxErr)
			}
			return s.stdout.Bytes(), s.stderr.Bytes(), commandErr
		}
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return s.stdout.Bytes(), s.stderr.Bytes(), errors.Join(
				fmt.Errorf("%s %s: %w", s.bin, strings.Join(s.args, " "), err), ctxErr)
		}
		return s.stdout.Bytes(), s.stderr.Bytes(),
			fmt.Errorf("%s %s: %w", s.bin, strings.Join(s.args, " "), err)
	}
	return s.stdout.Bytes(), s.stderr.Bytes(), nil
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

// diagnosticError keeps stdout available to classifiers without changing
// the historical four-field CLIError layout. Unwrap preserves errors.As
// and errors.Is behavior for the original CLIError.
type diagnosticError struct {
	cause  error
	cliErr *CLIError
	stdout string
}

func (e *diagnosticError) Error() string {
	msg := e.cliErr.Error()
	if e.stdout == "" {
		return msg
	}
	if e.cliErr.Stderr != "" {
		msg += "; stdout: "
	} else {
		msg += ": "
	}
	return msg + strings.TrimSpace(e.stdout)
}

func (e *diagnosticError) Unwrap() error { return e.cause }

func withStdout(err error, stdout string) error {
	if err == nil || stdout == "" {
		return err
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	var existing *diagnosticError
	if errors.As(err, &existing) && existing.cliErr == cliErr && existing.stdout != "" {
		return err
	}
	return &diagnosticError{cause: err, cliErr: cliErr, stdout: truncateOutput(stdout)}
}

// DiagnosticText returns the bounded diagnostic streams carried by a CLI
// failure. The bool reports whether err contains a CLIError.
// NewStreamCLIError builds a CLIError carrying a stdout diagnostic. A
// streamed command may report its failure on stdout, so the not-found and
// conflict matchers must see both streams.
func NewStreamCLIError(binary string, args []string, exitCode int, stdout, stderr string) error {
	return withStdout(&CLIError{
		Binary:   binary,
		Args:     args,
		ExitCode: exitCode,
		Stderr:   stderr,
	}, stdout)
}

func DiagnosticText(err error) (stdout, stderr string, ok bool) {
	if err == nil {
		return "", "", false
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return "", "", false
	}
	var diagnostic *diagnosticError
	if errors.As(err, &diagnostic) && diagnostic.cliErr == cliErr {
		stdout = diagnostic.stdout
	}
	return stdout, cliErr.Stderr, true
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

// Start spawns the child and returns without waiting, so a caller can drop
// a name lock as soon as the process exists.
func (r *ExecRunner) Start(ctx context.Context, args ...string) (*StartedCommand, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// If the process ignores the kill long enough to hold pipes open,
	// give up waiting shortly after.
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(
				fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err), ctxErr)
		}
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return &StartedCommand{cmd: cmd, bin: bin, args: args, stdout: &stdout, stderr: &stderr, ctx: ctx}, nil
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
				Stderr:   truncateStderr(stderr.String()),
			}
			commandErr := withStdout(cliErr, stdout.String())
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stdout.Bytes(), stderr.Bytes(), errors.Join(commandErr, ctxErr)
			}
			return stdout.Bytes(), stderr.Bytes(), commandErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), stderr.Bytes(), errors.Join(
				fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr), err)
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

func truncateStderr(s string) string { return truncateOutput(s) }

// IsCommandExit reports whether err is a CLIError from a child process
// that started and returned an exit status on its own. Launch failures
// (missing binary, OS exec errors) are not CLIErrors and return false.
//
// A context error in the chain vetoes the result: the child was signalled
// because the caller cancelled, so its -1 exit code is not an app outcome.
// Without the veto a killed child would be reported as "exit -1, no error"
// and a cancellation would silently become a successful command result.
func IsCommandExit(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var e *CLIError
	return errors.As(err, &e)
}

func collectCLIErrors(err error) []*CLIError {
	if err == nil {
		return nil
	}
	if cliErr, ok := err.(*CLIError); ok {
		return []*CLIError{cliErr}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []*CLIError
		for _, child := range joined.Unwrap() {
			out = append(out, collectCLIErrors(child)...)
		}
		return out
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return collectCLIErrors(wrapped.Unwrap())
	}
	return nil
}

// probeTimeout bounds the diagnostic liveness check so a hung backend
// cannot stall error handling forever. Caller cancellation still
// aborts the probe via context propagation.
const probeTimeout = 5 * time.Second

// probeDiagnostic carries a liveness-probe failure alongside the error it
// was investigating.
//
// It deliberately does NOT implement Unwrap. Chain walkers collect every
// *CLIError they can reach and veto on the first definitive non-liveness
// branch, so a probe that failed for an unrelated reason (a permission or
// certificate problem, say) would otherwise suppress ErrContainerNotFound
// for the original command. errors.Is still reaches the probe through Is,
// so callers keep full access to the probe's causes.
type probeDiagnostic struct {
	probe error
}

func (p probeDiagnostic) Error() string { return p.probe.Error() }

// Is forwards the probe's causes, except the context sentinels. The probe runs
// on its own internal budget, so its DeadlineExceeded is not the caller's
// deadline; leaking it would make hasDefinitiveErrorBranch treat the original
// error as definitively broken and suppress ErrContainerNotFound.
func (p probeDiagnostic) Is(target error) bool {
	if target == context.Canceled || target == context.DeadlineExceeded {
		return false
	}
	return errors.Is(p.probe, target)
}

// wrapProbe returns a probe failure that is visible to errors.Is but hidden
// from chain type walks over the original error.
func wrapProbe(probeErr error) error {
	if probeErr == nil {
		return nil
	}
	return probeDiagnostic{probe: probeErr}
}

// Classify augments a failed CLI call only when a probe proves that the
// backend is unavailable. Both the original and probe errors remain in the
// returned chain; a generic probe failure is never promoted to
// ErrSystemNotRunning.
func Classify(ctx context.Context, r Runner, err error, probe Probe) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(err, ctxErr)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || IsOperationTimeoutError(err) {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	probeStarted := time.Now()
	probeStdout, _, probeErr := r.Run(probeCtx, probe.Args...)
	if probeErr != nil {
		probeErr = withStdout(probeErr, string(probeStdout))
	}
	if probeErr == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(err, ctxErr)
		}
		return err
	}
	// Classify the raw probe, but publish it wrapped.
	diag := wrapProbe(probeErr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(err, diag, ctxErr)
	}
	// A precise client-side failure in the original command is not made
	// into daemon-down by an unrelated failed probe. An unqualified
	// application/run not-found is also ambiguous: it may describe the
	// workload rather than the backend daemon.
	if IsDefinitiveNonLivenessError(err) || containsAmbiguousNotFoundText(diagnosticText(err)) {
		return errors.Join(err, diag)
	}
	// A pure bounded-probe timeout is liveness evidence. A joined
	// permission/configuration/cancellation cause vetoes it.
	if isProbeTimeoutError(probeCtx, probeStarted, probeErr) && !probeVetoError(probeErr) {
		return classifySystemNotRunning(err, diag, probe.Hint)
	}
	if isProbeCancellationOrConfiguration(probeErr) {
		return errors.Join(err, diag)
	}
	if operationLivenessText(err) || probeUnavailable(probe, probeErr) {
		return classifySystemNotRunning(err, diag, probe.Hint)
	}
	return errors.Join(err, diag)
}

func classifySystemNotRunning(original, probeErr error, hint string) error {
	return errors.Join(
		fmt.Errorf("%w: %s", ErrSystemNotRunning, hint),
		original,
		probeErr,
	)
}

// isProbeTimeoutError reports a pure bounded-probe timeout: the probe
// context expired, the error carries that expiry, and the child was
// signalled. Run joins the context error onto any *exec.ExitError, so the
// DeadlineExceeded conjunct alone is not enough: a child that returned a
// real exit status just after the deadline failed on its own and must not
// be promoted to liveness evidence.
// isProbeTimeoutError reports that the liveness probe exhausted its own
// bounded budget.
//
// started is when the probe was launched. The probe context inherits the
// caller's deadline, so probeCtx.Err() can report DeadlineExceeded because the
// *caller* ran out of time before the probe consumed its own 5s. That is not
// evidence about the backend: a slow-but-healthy daemon would be reported as
// down, and callers branch on ErrSystemNotRunning.
func isProbeTimeoutError(probeCtx context.Context, started time.Time, err error) bool {
	if probeCtx.Err() != context.DeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Only the probe's own budget counts.
	if time.Since(started) < probeTimeout {
		return false
	}
	// A child that returned a real exit status is not timeout evidence: it
	// failed on its own, just after the deadline. A bare DeadlineExceeded
	// (no CLIError at all) means the child never produced an exit status,
	// which is exactly the hang this probe is looking for.
	for _, cliErr := range collectCLIErrors(err) {
		if cliErr.ExitCode >= 0 {
			return false
		}
	}
	return true
}

func probeVetoError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	for _, cliErr := range collectCLIErrors(err) {
		if cliErr.ExitCode == 126 || cliErr.ExitCode == 127 {
			return true
		}
	}
	return containsNonLivenessText(strings.ToLower(diagnosticText(err)))
}

// isProbeCancellationOrConfiguration reports an error that must never be
// read as liveness evidence: a definite client-side or configuration
// failure, or a cancellation. A bounded-probe timeout is excluded by the
// caller via the probe context, so every DeadlineExceeded reaching here is
// a real cancellation.
func isProbeCancellationOrConfiguration(err error) bool {
	return probeVetoError(err) || errors.Is(err, context.DeadlineExceeded)
}

func operationLivenessText(err error) bool {
	if err == nil {
		return false
	}
	if IsDefinitiveNonLivenessError(err) {
		return false
	}
	s := strings.ToLower(diagnosticText(err))
	for _, fragment := range []string{
		"xpc connection", "cannot connect", "connection refused",
		"system is not running", "daemon is not running", "backend down",
		"apiserver is not running", "not registered with launchd",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func probeUnavailable(probe Probe, err error) bool {
	if IsDefinitiveNonLivenessError(err) {
		return false
	}
	if probe.IsUnavailable != nil {
		return probe.IsUnavailable(err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	s := strings.ToLower(diagnosticText(err))
	for _, fragment := range []string{
		"cannot connect to the docker daemon",
		"cannot connect to the container backend",
		"connection refused",
		"xpc connection",
		"system is not running",
		"system service is not running",
		"apiserver is not running",
		"not registered with launchd",
		"daemon is not running",
		"is the docker daemon running",
		"failed to connect to the docker daemon", "backend down", "daemon unavailable",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func diagnosticText(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	var walk func(error)
	walk = func(cur error) {
		if cur == nil {
			return
		}
		if diagnostic, ok := cur.(*diagnosticError); ok {
			if diagnostic.stdout != "" {
				parts = append(parts, diagnostic.stdout)
			}
			walk(diagnostic.cause)
			return
		}
		if joined, ok := cur.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if wrapped, ok := cur.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
			return
		}
		var cliErr *CLIError
		if errors.As(cur, &cliErr) {
			if stdout, stderr, ok := DiagnosticText(cur); ok {
				if stdout != "" {
					parts = append(parts, stdout)
				}
				if stderr != "" {
					parts = append(parts, stderr)
				}
			}
			return
		}
		if text := cur.Error(); text != "" {
			parts = append(parts, text)
		}
	}
	walk(err)
	return endpointURI.ReplaceAllString(strings.Join(parts, "\n"), " ")
}

func containsNonLivenessText(s string) bool {
	for _, fragment := range []string{
		"permission denied", "operation not permitted", "access denied",
		"eacces", "eperm", "unauthorized", "forbidden",
		"authentication required", "credential helper", "credentials",
		"invalid config", "invalid configuration", "config error",
		"configuration file", "configuration directory", "failed to load config",
		"invalid context", "unknown context", "no such context",
		"tls handshake", "x509:", "certificate signed by unknown authority",
		"unknown flag", "unknown option", "invalid option", "invalid argument",
		"operation canceled", "operation cancelled", "context canceled",
		"context cancelled", "command canceled", "request canceled",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// containsAmbiguousNotFoundText reports unqualified absence text that
// may describe an application rather than the backend daemon.
func containsAmbiguousNotFoundText(s string) bool {
	for _, fragment := range []string{
		"not found", "container not found", "image not found",
		"no such container", "no such object", "no such image",
		"executable file not found", "no such file or directory",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// IsDefinitiveNonLivenessError reports a client-side failure that vetoes
// backend liveness and absence classification.
func IsDefinitiveNonLivenessError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	for _, cliErr := range collectCLIErrors(err) {
		if cliErr.ExitCode == 126 || cliErr.ExitCode == 127 {
			return true
		}
	}
	return containsNonLivenessText(strings.ToLower(diagnosticText(err)))
}

// IsNonLivenessError is the broader predicate used by backend probes.
// It also recognizes ambiguous object-absence diagnostics, which must not
// be promoted to a daemon-down result by a backend-specific matcher.
func IsNonLivenessError(err error) bool {
	return IsDefinitiveNonLivenessError(err) || containsAmbiguousNotFoundText(diagnosticText(err))
}

// IsOperationTimeoutError reports structured timeout/cancellation evidence
// without treating arbitrary application stderr as an operation timeout.
func IsOperationTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	for _, cliErr := range collectCLIErrors(err) {
		if cliErr.ExitCode < 0 {
			return true
		}
	}
	return false
}
