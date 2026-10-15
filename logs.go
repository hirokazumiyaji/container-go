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
// the timestamp. Each backend maps the options to its supported CLI arguments.
type LogsOptions struct {
	Tail  int
	Since time.Time
}

func (o LogsOptions) validate() error {
	if o.Tail < 0 {
		return validationErrorf("LogsOptions.Tail", o.Tail, "log tail must be >= 0, got %d", o.Tail)
	}
	return nil
}

// Logs returns a snapshot of the container's log output so far.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	return c.LogsWithOptions(ctx, LogsOptions{})
}

// LogsWithOptions returns a bounded snapshot of the container's log
// output. Long-lived reuse containers can grow unbounded logs, so
// prefer Tail for diagnostics. It returns ErrUnsupportedCapability
// without invoking the backend when the backend cannot honor opts.
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
	stdout, stderr, err := c.runner.Run(qCtx, args...)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, wrapNotFoundFor(c.eng, c.classify(ctx, err))
	}
	// docker logs splits the container's streams across the CLI's
	// stdout and stderr; a snapshot carries both.
	return io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))), nil
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
