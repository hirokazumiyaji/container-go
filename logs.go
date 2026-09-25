package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// LogsOptions bounds a Logs snapshot. Tail keeps the last N lines
// (0 means all); Since drops entries older than the timestamp. Both
// map to the backend CLI's --tail/--since flags.
type LogsOptions struct {
	Tail  int
	Since time.Time
}

func (o LogsOptions) args() []string {
	var args []string
	if o.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(o.Tail))
	}
	if !o.Since.IsZero() {
		args = append(args, "--since", o.Since.Format(time.RFC3339))
	}
	return args
}

// Logs returns a snapshot of the container's log output so far.
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error) {
	return c.LogsWithOptions(ctx, LogsOptions{})
}

// LogsWithOptions returns a bounded snapshot of the container's log
// output. Long-lived reuse containers can grow unbounded logs, so
// prefer Tail for diagnostics.
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	args := c.eng.logsArgs(c.id, false)
	if extra := opts.args(); len(extra) > 0 {
		// Insert --tail/--since before the container ID (last arg).
		args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
	}
	stdout, stderr, err := c.runner.Run(qCtx, args...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	// docker logs splits the container's streams across the CLI's
	// stdout and stderr; a snapshot carries both.
	return io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))), nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process. A startup failure is returned by FollowLogs; after the stream
// is returned, a terminal CLI failure is delivered by Read. The direct
// CLI child is always reaped. Windows may terminate descendants attached
// to its Job Object; Unix guarantees only the direct child.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	stream, err := s.Stream(ctx, c.eng.logsArgs(c.id, true)...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	return &classifyingStream{
		ReadCloser: stream,
		ctx:        ctx,
		container:  c,
	}, nil
}

type classifyingStream struct {
	io.ReadCloser
	ctx       context.Context
	container *Container

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
	s.terminalOnce.Do(func() {
		status, ok := s.ReadCloser.(terminalStreamStatus)
		if !ok {
			return
		}
		s.terminalErr = s.wrap(status.TerminalError())
	})
	return s.terminalErr
}

func (s *classifyingStream) wrap(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	classified := s.container.classify(s.ctx, err)
	if classified == nil || errors.Is(classified, err) || errors.Is(err, classified) {
		return wrapNotFound(classified)
	}
	// A backend probe can classify a terminal CLI failure as an
	// infrastructure problem. Keep both causes so Read/ForLog callers can
	// still recover CLIError and ErrContainerNotFound with errors.Is/As.
	return wrapNotFound(errors.Join(classified, err))
}
