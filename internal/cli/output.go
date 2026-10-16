package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

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
	if e == nil || e.Err == nil {
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

// joinErrors joins the non-nil errors without wrapping a lone error, so a
// direct *CLIError stays distinguishable from one joined to a sink error.
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

func writerTruncated(w io.Writer) bool {
	type truncationReporter interface {
		Truncated() bool
	}
	tr, ok := w.(truncationReporter)
	return ok && tr.Truncated()
}

func outputDeliveryError(stream string, err error) error {
	if err == nil {
		return nil
	}
	return &OutputError{Stream: stream, Err: err}
}

// countingWriter counts every byte the child produced and forwards it to
// dst until dst fails. After the first failure the child is still drained
// and counted, but dst is not written again: a failed sink must not be
// handed further output.
type countingWriter struct {
	mu    sync.Mutex
	dst   io.Writer
	bytes int64
	err   error
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bytes += int64(len(p))
	if w.err != nil {
		return len(p), nil
	}

	n, err := w.dst.Write(p)
	if n < 0 || n > len(p) {
		err = fmt.Errorf("invalid write count %d for %d-byte buffer", n, len(p))
	} else if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
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
	_, _ = w.diagnostic.Write(p)
	return len(p), nil
}
