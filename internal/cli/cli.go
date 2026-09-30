// Package cli executes a container backend CLI (`container` or `docker`)
// as a child process and classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// maxStderr is the maximum number of trailing stderr bytes ExecRunner
// retains when constructing one CLIError diagnostic. It does not bound
// the output returned by Run or the caller-owned sinks passed to RunTo;
// those paths keep their own contract. Custom Runner implementations
// remain responsible for applying their own diagnostic bound.
const maxStderr = 64 * 1024

// ErrOutputDelivery identifies a failure to deliver CLI output to a
// caller-provided sink. The underlying writer error remains in the
// error chain for errors.Is/errors.As checks.
var ErrOutputDelivery = errors.New("CLI output delivery failed")

// OutputError annotates a sink failure with the CLI stream that could not
// be written. It is kept in internal/cli because callers should normally
// inspect their own writer error rather than depend on this wrapper type.
type OutputError struct {
	Stream string
	Err    error
}

func (e *OutputError) Error() string {
	if e == nil {
		return ErrOutputDelivery.Error()
	}
	if e.Err == nil {
		return ErrOutputDelivery.Error()
	}
	if e.Stream == "" {
		return fmt.Sprintf("%s: %v", ErrOutputDelivery, e.Err)
	}
	return fmt.Sprintf("%s: write CLI %s: %v", ErrOutputDelivery, e.Stream, e.Err)
}

// Unwrap exposes the original writer error. Is handles the delivery
// sentinel as well, so errors.Is works for either one without requiring
// callers to know about this wrapper type.
func (e *OutputError) Unwrap() error {
	if e == nil || e.Err == nil {
		return ErrOutputDelivery
	}
	return e.Err
}

func (e *OutputError) Is(target error) bool {
	return target == ErrOutputDelivery || (e != nil && e.Err != nil && errors.Is(e.Err, target))
}

// IsOutputError reports whether err contains a caller-sink delivery
// failure. WriterRunner implementations should return OutputError (or an
// error chain containing it) when a sink rejects output.
func IsOutputError(err error) bool {
	var outputErr *OutputError
	return errors.As(err, &outputErr)
}

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

// OutputStats describes bytes observed by RunTo. The counts include
// bytes that a caller-supplied writer subsequently discarded.
type OutputStats struct {
	StdoutBytes     int64
	StderrBytes     int64
	StdoutTruncated bool
	StderrTruncated bool
}

// Truncated reports whether either output stream was truncated by a
// writer passed to RunTo.
func (s OutputStats) Truncated() bool {
	return s.StdoutTruncated || s.StderrTruncated
}

// WriterRunner is the streaming form of Runner. Implementations write
// directly to the supplied sinks, so a sink can keep a fixed-size view
// without first allocating the complete CLI output. A sink failure must
// be represented in the returned error (preferably as *OutputError), even
// when the child exits successfully; a terminal CLIError and a sink error
// should be joined rather than one replacing the other. Runner remains
// separate for compatibility with existing test and embedding runners.
type WriterRunner interface {
	Runner
	RunTo(ctx context.Context, stdout, stderr io.Writer, args ...string) (OutputStats, error)
}

// RunTo executes a CLI invocation through WriterRunner when available.
// Runners that only implement the historical Runner interface are
// adapted for compatibility; that fallback necessarily has already
// materialized their returned byte slices. A sink failure is returned
// together with the CLI's terminal error, rather than replacing either
// one or being mistaken for a successful invocation.
func RunTo(r Runner, ctx context.Context, stdout, stderr io.Writer, args ...string) (OutputStats, error) {
	if wr, ok := r.(WriterRunner); ok {
		return wr.RunTo(ctx, stdout, stderr, args...)
	}

	dataOut, dataErr, runErr := r.Run(ctx, args...)
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	// Try both streams even if the first sink fails. Both failures are
	// useful to a caller, and observing stderr does not change the
	// already-materialized legacy-runner result.
	var deliveryErr error
	if err := writeOutput(stdout, dataOut); err != nil {
		deliveryErr = joinErrors(deliveryErr, &OutputError{Stream: "stdout", Err: err})
	}
	if err := writeOutput(stderr, dataErr); err != nil {
		deliveryErr = joinErrors(deliveryErr, &OutputError{Stream: "stderr", Err: err})
	}
	return OutputStats{
		StdoutBytes:     int64(len(dataOut)),
		StderrBytes:     int64(len(dataErr)),
		StdoutTruncated: writerTruncated(stdout),
		StderrTruncated: writerTruncated(stderr),
	}, joinErrors(runErr, deliveryErr)
}

func writeOutput(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("invalid write count %d for %d-byte buffer", n, len(data))
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func joinErrors(errs ...error) error {
	nonNil := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			nonNil = append(nonNil, err)
		}
	}
	switch len(nonNil) {
	case 0:
		return nil
	case 1:
		return nonNil[0]
	default:
		return errors.Join(nonNil...)
	}
}

func writerTruncated(w io.Writer) bool {
	type truncationReporter interface {
		Truncated() bool
	}
	tr, ok := w.(truncationReporter)
	return ok && tr.Truncated()
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
	var stdout, stderr bytes.Buffer
	_, err := r.RunTo(ctx, &stdout, &stderr, args...)
	// The historical Runner contract returns the complete output. The
	// bounded/streaming contract is available through RunTo and does
	// not change this compatibility path.
	return stdout.Bytes(), stderr.Bytes(), err
}

// RunTo runs the CLI while sending stdout and stderr directly to the
// supplied writers. The process is still fully drained after a sink
// reaches its own limit, so a bounded sink cannot deadlock the child.
// Sink errors are drained and remembered rather than returned from the
// write itself; the final error still contains them together with the
// process's terminal status.
func (r *ExecRunner) RunTo(ctx context.Context, stdout, stderr io.Writer, args ...string) (OutputStats, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stdoutCount := &countingWriter{dst: stdout}
	stderrCount := &countingWriter{dst: stderr}
	diagnostic := newTailBuffer(maxStderr)
	cmd.Stdout = stdoutCount
	cmd.Stderr = &diagnosticWriter{output: stderrCount, diagnostic: diagnostic}
	// If the process ignores the kill long enough to hold pipes open,
	// give up waiting shortly after.
	cmd.WaitDelay = 3 * time.Second

	runErr := cmd.Run()
	stdoutBytes, stdoutErr := stdoutCount.snapshot()
	stderrBytes, stderrErr := stderrCount.snapshot()
	stats := OutputStats{
		StdoutBytes:     stdoutBytes,
		StderrBytes:     stderrBytes,
		StdoutTruncated: writerTruncated(stdout),
		StderrTruncated: writerTruncated(stderr),
	}
	terminalErr := commandError(ctx, bin, args, cmd.ProcessState, runErr, diagnostic.String())
	deliveryErr := joinErrors(
		outputDeliveryError("stdout", stdoutErr),
		outputDeliveryError("stderr", stderrErr),
	)
	return stats, joinErrors(terminalErr, deliveryErr)
}

// commandError builds the terminal CLI error after the child has been
// reaped. A sink is deliberately drained even after it fails, so a
// non-zero process status remains available to callers instead of being
// hidden by os/exec's copy error.
func commandError(ctx context.Context, bin string, args []string, state *os.ProcessState, runErr error, stderr string) error {
	var terminal error
	var exitErr *exec.ExitError
	hasExitError := errors.As(runErr, &exitErr)
	if state != nil && state.ExitCode() > 0 {
		terminal = &CLIError{
			Binary:   bin,
			Args:     args,
			ExitCode: state.ExitCode(),
			Stderr:   truncateStderr(stderr),
		}
	} else if hasExitError && exitErr.ExitCode() >= 0 {
		terminal = &CLIError{
			Binary:   bin,
			Args:     args,
			ExitCode: exitErr.ExitCode(),
			Stderr:   truncateStderr(stderr),
		}
	}
	if terminal != nil {
		// A process-state error and the os/exec error describe the same
		// exit status. Keep unrelated wait/transport errors as well.
		if runErr != nil && !hasExitError {
			terminal = joinErrors(terminal, runErr)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			terminal = joinErrors(terminal, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr))
		}
		return terminal
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinErrors(fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctxErr), runErr)
	}
	return runErr
}

func outputDeliveryError(stream string, err error) error {
	if err == nil {
		return nil
	}
	return &OutputError{Stream: stream, Err: err}
}

type countingWriter struct {
	mu    sync.Mutex
	dst   io.Writer
	bytes int64
	err   error
}

func (w *countingWriter) Write(p []byte) (int, error) {
	// Count the bytes observed from the child, even when the destination
	// rejects them. The child is drained by returning a complete count.
	w.mu.Lock()
	w.bytes += int64(len(p))
	w.mu.Unlock()

	n, err := w.dst.Write(p)
	if n < 0 || n > len(p) {
		err = fmt.Errorf("invalid write count %d for %d-byte buffer", n, len(p))
	} else if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.recordError(err)
	}
	// Returning the full input count is intentional: a sink failure must
	// not stop os/exec's copy pump and leave a child blocked on a pipe.
	return len(p), nil
}

func (w *countingWriter) snapshot() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes, w.err
}

// diagnosticWriter forwards stderr to the caller and independently
// retains the bounded diagnostic copy. A failed caller sink must not
// prevent the diagnostic copy from seeing the CLI's terminal message.
type diagnosticWriter struct {
	output     *countingWriter
	diagnostic io.Writer
}

func (w *diagnosticWriter) Write(p []byte) (int, error) {
	_, _ = w.output.Write(p)
	if _, err := w.diagnostic.Write(p); err != nil {
		w.output.recordError(err)
	}
	return len(p), nil
}

func (w *countingWriter) recordError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	// A failing writer may return a fresh wrapper for every Write. Keep
	// the first representative so repeated output cannot grow the error.
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}

// tailBuffer retains the trailing max bytes for one CLIError diagnostic.
// It always reports complete writes, so diagnostic capture cannot block
// the child or hide a later terminal error.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (w *tailBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.max <= 0 {
		return len(p), nil
	}
	if len(p) >= w.max {
		if cap(w.data) < w.max {
			w.data = make([]byte, w.max)
		} else {
			w.data = w.data[:w.max]
		}
		copy(w.data, p[len(p)-w.max:])
		return len(p), nil
	}
	overflow := len(w.data) + len(p) - w.max
	if overflow > 0 {
		copy(w.data, w.data[overflow:])
		w.data = w.data[:len(w.data)-overflow]
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *tailBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.data...))
}

// truncateStderr is the final safety bound for a diagnostic assembled
// by a custom runner. The normal ExecRunner path already uses a tail.
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
// the probe, the failure is reported as ErrSystemNotRunning without
// discarding the original CLI, sink, or probe errors.
func Classify(ctx context.Context, r Runner, err error, probe Probe) error {
	if err == nil {
		return nil
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Caller already gave up; retain both facts even when a runner
		// returned only the command error.
		return joinErrors(err, ctxErr)
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, _, probeErr := r.Run(probeCtx, probe.Args...); probeErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return joinErrors(err, probeErr, ctxErr)
		}
		return fmt.Errorf("%w: %s (underlying error: %w)", ErrSystemNotRunning, probe.Hint, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinErrors(err, ctxErr)
	}
	return err
}
