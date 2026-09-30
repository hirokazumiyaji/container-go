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
	// OperationTimeout is structured evidence supplied by the lifecycle or
	// runner. Free-form stderr is application output and is not used to
	// classify an operation timeout.
	OperationTimeout bool
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
// argv vector; no shell is involved. Cancellation terminates the lifecycle-
// owned local process tree; it does not claim to terminate a process inside
// a backend.
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
	configureProcessTree(cmd)
	lifecycle := newCommandLifecycle(ctx, cmd)
	cmd.Cancel = lifecycle.cancel
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// If the process ignores the kill long enough to hold pipes open,
	// give up waiting shortly after.
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		lifecycle.failStart()
		return stdout.Bytes(), stderr.Bytes(), markRunStatus(err, lifecycle.status())
	}
	// On Windows, newProcessTree attaches the Job Object after Start; the
	// short attachment-window limitation is documented by that platform
	// implementation.
	tree, treeErr := newProcessTree(cmd)
	if treeErr != nil {
		// A platform tree is an enhancement. The direct process handle
		// remains a safe cancellation path when attachment is unavailable.
		tree = directProcessTree{}
	}
	lifecycle.publishStart(tree)

	// Run owns the sole Wait call through the same lifecycle used by Stream.
	// This keeps context cancellation and the eventual reap from racing a
	// stale numeric process-group signal.
	err := lifecycle.result()
	// Output buffers are returned whole: success output and non-zero
	// exec/log results must not be silently truncated. Only the
	// diagnostic copy inside CLIError is bounded.
	commandErr := commandError(ctx, bin, args, stderr.Bytes(), err)
	return stdout.Bytes(), stderr.Bytes(), markRunStatus(commandErr, lifecycle.status())
}

func commandError(ctx context.Context, bin string, args []string, stderr []byte, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode := exitErr.ExitCode()
		if ctxErr := ctx.Err(); ctxErr != nil && exitCode < 0 {
			// A Unix signal has no usable application status. Preserve the
			// context contract while the lifecycle status records whether
			// local cancellation actually terminated the process.
			return fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr)
		}
		operationTimeout := errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), os.ErrDeadlineExceeded)
		cliErr := &CLIError{
			Binary:           bin,
			Args:             args,
			ExitCode:         exitCode,
			Stderr:           truncateStderr(string(stderr)),
			OperationTimeout: operationTimeout,
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The process did produce an exit status, but cancellation
			// raced with its completion. Keep both facts observable.
			return errors.Join(cliErr, ctxErr)
		}
		return cliErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr)
	}
	return err
}

// IsOperationTimeoutError reports whether err has structured timeout,
// cancellation, or signal evidence rather than an application result.
// Free-form command stderr is never used as timeout evidence.
func IsOperationTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}

	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		// A non-CLI error has no structured timeout evidence. Do not infer
		// one from its free-form Error text.
		return false
	}
	// A negative status is the standard-library representation of a
	// signal. It is not an application exit result.
	if cliErr.ExitCode < 0 {
		return true
	}
	// The lifecycle may attach structured timeout evidence while preserving
	// a positive application exit status. Never infer classification from
	// arbitrary workload stderr.
	return cliErr.OperationTimeout
}

// truncateStderr bounds the diagnostic copy kept in CLIError. Keep the
// tail so a final timeout or daemon diagnostic is not hidden behind a
// large amount of preceding application output.
func truncateStderr(s string) string {
	if len(s) > maxStderr {
		return s[len(s)-maxStderr:]
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

// Classify augments a failed CLI call: if the backend does not answer
// the probe, the failure is reported as ErrSystemNotRunning instead of
// the original error.
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
	// A timeout or signal reported by the operation itself is already a
	// known termination result. Probing the backend after it would turn
	// a useful operation error into a second, misleading operation.
	if IsOperationTimeoutError(err) {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, _, probeErr := r.Run(probeCtx, probe.Args...); probeErr != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: %s (underlying error: %v)", ErrSystemNotRunning, probe.Hint, err)
	}
	return err
}
