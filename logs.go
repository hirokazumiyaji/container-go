package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// LogsOptions bounds a Logs snapshot. Tail keeps the last N lines
// (it must be non-negative; 0 means all); Since drops entries older than
// the timestamp. Each backend maps those options to its supported CLI
// arguments. A positive MaxBytes caps the combined stdout+stderr response;
// zero preserves the historical unbounded snapshot behavior.
//
// Tail is a backend line-based tail. MaxBytes is applied while the CLI
// runs: it retains the prefix of the combined output, not the trailing
// bytes. The added field intentionally makes keyed literals the required
// form; old two-field unkeyed literals no longer compile, which is the
// explicit pre-1.0 compatibility tradeoff.
type LogsOptions struct {
	Tail     int
	Since    time.Time
	MaxBytes int64
}

func (o LogsOptions) validate() error {
	if o.Tail < 0 {
		return validationErrorf("LogsOptions.Tail", o.Tail, "log tail must be >= 0, got %d", o.Tail)
	}
	if o.MaxBytes < 0 {
		return validationErrorf("LogsOptions.MaxBytes", o.MaxBytes, "log MaxBytes must be >= 0, got %d", o.MaxBytes)
	}
	return nil
}

// Logs returns a snapshot of the container's log output so far. If the
// CLI fails, any bytes it supplied are still returned with the error.
//
// Deprecated: use LogsWithOptions with a positive MaxBytes or LogsTo for
// output that may be large or untrusted. This compatibility form keeps
// the historical unbounded capture.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	return c.LogsWithOptions(ctx, LogsOptions{})
}

// LogsWithOptions returns a snapshot of the container's log output.
// Long-lived reuse containers can grow unbounded logs, so prefer Tail or
// MaxBytes for diagnostics. A positive MaxBytes enables a fixed-size
// prefix capture of the merged stdout and stderr; zero retains the
// historical full-snapshot behavior. It returns ErrUnsupportedCapability
// without invoking the backend when the backend cannot honor opts.
//
// When the CLI fails after producing output, the returned reader is still
// non-nil and contains that output together with the error; a bounded
// reader retains its truncation state.
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	target, err := c.verifiedOperationTarget(qCtx)
	if err != nil {
		return nil, err
	}
	args, err := c.eng.logsArgsWithOptions(target, opts)
	if err != nil {
		return nil, err
	}

	var (
		output io.ReadCloser
		runErr error
	)
	if opts.MaxBytes > 0 {
		capture := newBoundedBuffer(opts.MaxBytes)
		_, runErr = cli.RunTo(c.runner, qCtx, capture, capture, args...)
		output = &boundedReadCloser{boundedReader: capture.reader()}
	} else {
		var stdout, stderr []byte
		stdout, stderr, runErr = c.runner.Run(qCtx, args...)
		// docker logs splits the container's streams across the CLI's
		// stdout and stderr; a snapshot carries both.
		output = io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)))
	}
	if runErr != nil {
		return output, c.logsError(ctx, qCtx, runErr)
	}
	return output, nil
}

// LogsTo streams a log snapshot directly to output. When MaxBytes is
// positive, the first MaxBytes bytes of the combined stream are forwarded;
// later bytes are drained and discarded rather than buffered. The
// returned stats count all bytes observed by the CLI. A sink failure is
// returned together with the CLI's terminal error, if any.
func (c *Container) LogsTo(ctx context.Context, output io.Writer, opts LogsOptions) (OutputStats, error) {
	if err := opts.validate(); err != nil {
		return OutputStats{}, err
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	target, err := c.verifiedOperationTarget(qCtx)
	if err != nil {
		return OutputStats{}, err
	}
	args, err := c.eng.logsArgsWithOptions(target, opts)
	if err != nil {
		return OutputStats{}, err
	}

	tracked := newOutputTrackingWriter(output)
	sink, limited := outputSink(tracked, opts.MaxBytes)
	stats, runErr := cli.RunTo(c.runner, qCtx, sink, sink, args...)
	runErr = preserveOutputDeliveryError(runErr, tracked)
	result := newOutputStats(stats, limited)
	if runErr != nil {
		return result, c.logsError(ctx, qCtx, runErr)
	}
	return result, nil
}

func (c *Container) logsError(ctx, qCtx context.Context, err error) error {
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return contextErr
	}
	return preserveError(wrapNotFoundFor(c.eng, c.classify(ctx, err)), err)
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process. A startup failure is returned by FollowLogs; after the stream
// is returned, a terminal CLI failure is delivered by Read. The direct
// CLI child is always reaped. Unix process groups provide best-effort
// descendant termination while that child is owned. Windows uses the
// retained process handle for direct-child termination; detached
// descendants are not terminated. Once the child is reaped, Close does
// not signal its former process group.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	target, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return nil, err
	}
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, fmt.Errorf("logs: %w: runner does not support streaming", cli.ErrStreamSetup)
	}
	stream, err := s.Stream(ctx, c.eng.logsFollowArgs(target)...)
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		if stream != nil {
			_ = stream.Close()
		}
		return nil, contextErr
	}
	if err != nil {
		return nil, wrapNotFoundFor(c.eng, c.classify(ctx, err))
	}
	classified := &classifyingStream{
		ReadCloser: stream,
		ctx:        ctx,
		container:  c,
	}
	status, ok := stream.(terminalStreamStatus)
	if !ok || status.Done() == nil {
		return classified, nil
	}
	return &classifyingStatusStream{
		classifyingStream: classified,
		status:            status,
	}, nil
}

type classifyingStream struct {
	io.ReadCloser
	ctx          context.Context
	container    *Container
	terminalOnce sync.Once
	terminalErr  error
}

type terminalStreamStatus interface {
	Done() <-chan struct{}
	TerminalError() error
}

func (s *classifyingStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if _, ok := s.ReadCloser.(terminalStreamStatus); ok {
		if terminalErr := s.TerminalError(); terminalErr != nil {
			return n, terminalErr
		}
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		return n, s.classifyTerminal(err)
	}
	return n, s.wrap(err)
}

// Done forwards the optional process status exposed by the underlying stream.
// A nil channel means that the reader has no status capability; wait.ForLog
// then falls back to its ordinary io.Reader contract.
func (s *classifyingStream) Done() <-chan struct{} {
	if status, ok := s.ReadCloser.(interface{ Done() <-chan struct{} }); ok {
		return status.Done()
	}
	return nil
}

// TerminalError returns the classified terminal CLI error, if the stream
// implementation exposes one. It is cached so Read and readiness handling
// observe the same cause and classification probe result.
func (s *classifyingStream) TerminalError() error {
	if status, ok := s.ReadCloser.(interface{ TerminalError() error }); ok {
		return s.classifyTerminal(status.TerminalError())
	}
	return s.terminalErr
}

func (s *classifyingStream) classifyTerminal(err error) error {
	s.terminalOnce.Do(func() {
		s.terminalErr = s.wrap(err)
	})
	return s.terminalErr
}

type classifyingStatusStream struct {
	*classifyingStream
	status terminalStreamStatus
}

func (s *classifyingStatusStream) Done() <-chan struct{} {
	return s.status.Done()
}

func (s *classifyingStatusStream) TerminalError() error {
	return s.classifyTerminal(s.status.TerminalError())
}

// Drain forwards the optional process-stream drain operation so wait.ForLog
// can finish stderr capture before classifying a terminal CLI error.
func (s *classifyingStatusStream) Drain(ctx context.Context) error {
	if drainer, ok := s.ReadCloser.(interface{ Drain(context.Context) error }); ok {
		return drainer.Drain(ctx)
	}
	return nil
}

func (s *classifyingStream) wrap(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	classified := s.container.classify(s.ctx, err)
	if classified == nil || errors.Is(classified, err) || errors.Is(err, classified) {
		return wrapNotFoundFor(s.container.eng, classified)
	}
	// A backend probe can classify a terminal CLI failure as an
	// infrastructure problem. Keep both causes so Read/ForLog callers can
	// still recover CLIError and ErrContainerNotFound with errors.Is/As.
	return wrapNotFoundFor(s.container.eng, errors.Join(classified, err))
}
