// Package cli executes a container backend CLI (`container` or `docker`)
// as a child process and classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
// without first allocating the complete CLI output. Runner remains
// separate for compatibility with existing test and embedding runners.
type WriterRunner interface {
	Runner
	RunTo(ctx context.Context, stdout, stderr io.Writer, args ...string) (OutputStats, error)
}

// RunTo executes a CLI invocation through WriterRunner when available.
// Runners that only implement the historical Runner interface are
// adapted for compatibility; that fallback necessarily has already
// materialized their returned byte slices.
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
	stats := OutputStats{
		StdoutBytes:     int64(len(dataOut)),
		StderrBytes:     int64(len(dataErr)),
		StdoutTruncated: writerTruncated(stdout),
		StderrTruncated: writerTruncated(stderr),
	}
	if err := writeOutput(stdout, dataOut); err != nil {
		return stats, fmt.Errorf("write CLI stdout: %w", err)
	}
	if err := writeOutput(stderr, dataErr); err != nil {
		return stats, fmt.Errorf("write CLI stderr: %w", err)
	}
	stats.StdoutTruncated = writerTruncated(stdout)
	stats.StderrTruncated = writerTruncated(stderr)
	return stats, runErr
}

func writeOutput(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
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
	diagnostic := &headWriter{max: maxStderr}
	cmd.Stdout = stdoutCount
	cmd.Stderr = io.MultiWriter(stderrCount, diagnostic)
	// If the process ignores the kill long enough to hold pipes open,
	// give up waiting shortly after.
	cmd.WaitDelay = 3 * time.Second

	runErr := cmd.Run()
	stats := OutputStats{
		StdoutBytes:     stdoutCount.bytes,
		StderrBytes:     stderrCount.bytes,
		StdoutTruncated: writerTruncated(stdout),
		StderrTruncated: writerTruncated(stderr),
	}
	// Output sinks are intentionally not allowed to hide the command's
	// terminal status. The diagnostic stderr copy is bounded separately
	// from the caller's sink.
	if runErr != nil {
		if ctx.Err() != nil {
			return stats, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return stats, &CLIError{
				Binary:   bin,
				Args:     args,
				ExitCode: exitErr.ExitCode(),
				Stderr:   truncateStderr(diagnostic.String()),
			}
		}
		return stats, runErr
	}
	return stats, nil
}

type countingWriter struct {
	dst   io.Writer
	bytes int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.bytes += int64(len(p))
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// headWriter retains only the first max bytes for CLIError diagnostics.
type headWriter struct {
	buf bytes.Buffer
	max int
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			_, _ = w.buf.Write(p[:room])
		} else {
			_, _ = w.buf.Write(p)
		}
	}
	return len(p), nil
}

func (w *headWriter) String() string { return w.buf.String() }

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
