// Package cli executes a container backend CLI (`container` or `docker`)
// as a child process and classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
//
// Error renders a safe diagnostic: secret-shaped arguments and stderr values
// are redacted, and control characters are escaped. RawError is the explicit
// unredacted local-debugging escape hatch. The unexported fields retain the
// redactor and original source when an error is obtained through a safe
// wrapper; callers should use keyed literals when constructing this public
// compatibility type.
type CLIError struct {
	// Binary is the CLI executable that failed (e.g. "container" or
	// "docker"). Empty means the historical default of "container".
	Binary   string
	Args     []string
	ExitCode int
	Stderr   string

	redactor *Redactor
	raw      *CLIError
}

// Error renders a safe diagnostic. Secret-shaped arguments and stderr
// values are redacted, and control characters are escaped so the result
// cannot alter a terminal. Use RawError only when the unredacted text is
// explicitly needed and the caller will keep it out of logs.
func (e *CLIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.redactor != nil {
		return e.format(e.redactor)
	}
	return e.format(NewRedactor())
}

// RawError returns the original command and stderr without redaction or
// control-character escaping. It is an explicit escape hatch for local
// debugging; do not write the result to CI logs or issue reports.
func (e *CLIError) RawError() string {
	if e == nil {
		return "<nil>"
	}
	raw := e.original()
	bin := raw.Binary
	if bin == "" {
		bin = "container"
	}
	msg := fmt.Sprintf("%s %s: exit code %d", bin, strings.Join(raw.Args, " "), raw.ExitCode)
	if raw.Stderr != "" {
		msg += ": " + raw.Stderr
	}
	return msg
}

func (e *CLIError) Is(target error) bool {
	other, ok := target.(*CLIError)
	return ok && e != nil && other != nil && e.original() == other.original()
}

func (e *CLIError) Unwrap() error {
	if e == nil || e.raw == nil {
		return nil
	}
	return e.raw
}

func (e *CLIError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}

func (e *CLIError) original() *CLIError {
	if e == nil {
		return nil
	}
	if e.raw != nil {
		return e.raw.original()
	}
	return e
}

func (e *CLIError) format(r *Redactor) string {
	raw := e.original()
	bin := raw.Binary
	if bin == "" {
		bin = "container"
	}
	args := r.Args(raw.Args)
	msg := fmt.Sprintf("%s %s: exit code %d", r.Text(bin), strings.Join(args, " "), raw.ExitCode)
	stderr := truncateStderr(r.Text(raw.Stderr))
	if stderr != "" {
		msg += ": " + strings.TrimSpace(stderr)
	}
	return msg
}

func (e *CLIError) withRedactor(r *Redactor) *CLIError {
	if r == nil {
		r = NewRedactor()
	}
	raw := e.original()
	combined := r
	if e.redactor != nil {
		combined = e.redactor.Compose(r)
	}
	return &CLIError{
		Binary:   combined.Text(raw.Binary),
		Args:     combined.Args(raw.Args),
		ExitCode: raw.ExitCode,
		Stderr:   truncateStderr(combined.Text(raw.Stderr)),
		redactor: combined,
		raw:      raw,
	}
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
		if ctx.Err() != nil {
			safeArgs := NewRedactor().Args(args)
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s %s: %w", bin, strings.Join(safeArgs, " "), ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), stderr.Bytes(), &CLIError{
				Binary:   bin,
				Args:     args,
				ExitCode: exitErr.ExitCode(),
				Stderr:   truncateStderr(stderr.String()),
			}
		}
		return stdout.Bytes(), stderr.Bytes(), err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
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

// SystemNotRunningError reports a failed backend operation whose liveness
// probe also failed. It retains all three relevant errors in its unwrap
// chain: ErrSystemNotRunning, the original operation error, and the probe
// error. The latter two are available explicitly as well, which is useful
// when both are *CLIError values.
type SystemNotRunningError struct {
	hint     string
	original error
	probe    error
}

// Error renders the classification and both underlying diagnostics. Public
// callers normally receive this through cli.WithRedactor, so configured
// values are sanitized at the boundary.
func (e *SystemNotRunningError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("%s: %s", ErrSystemNotRunning, e.hint)
	if e.original != nil {
		msg += fmt.Sprintf(" (underlying error: %v)", e.original)
	}
	if e.probe != nil {
		msg += fmt.Sprintf(" (probe error: %v)", e.probe)
	}
	return msg
}

// Unwrap exposes the sentinel, original error, and probe error to errors.Is
// and errors.As.
func (e *SystemNotRunningError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := []error{ErrSystemNotRunning}
	if e.original != nil {
		errs = append(errs, e.original)
	}
	if e.probe != nil {
		errs = append(errs, e.probe)
	}
	return errs
}

// OriginalError returns the failed operation that triggered the probe.
func (e *SystemNotRunningError) OriginalError() error {
	if e == nil {
		return nil
	}
	return e.original
}

// ProbeError returns the failed liveness probe.
func (e *SystemNotRunningError) ProbeError() error {
	if e == nil {
		return nil
	}
	return e.probe
}

// Classify augments a failed CLI call: if the backend does not answer the
// probe, the failure is reported as ErrSystemNotRunning while retaining both
// the original and probe errors in the chain.
func Classify(ctx context.Context, r Runner, err error, probe Probe) error {
	if err == nil {
		return nil
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	if ctx.Err() != nil {
		// Caller already gave up; preserve the original failure
		// instead of masking it with a probe cancellation.
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, _, probeErr := r.Run(probeCtx, probe.Args...); probeErr != nil {
		if ctx.Err() != nil {
			return err
		}
		return &SystemNotRunningError{hint: probe.Hint, original: err, probe: probeErr}
	}
	return err
}
