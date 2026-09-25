package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, fmt.Errorf("logs: %w: runner does not support streaming", cli.ErrStreamSetup)
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
	ctx          context.Context
	container    *Container
	terminalOnce sync.Once
	terminalErr  error
}

func (s *classifyingStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		return n, s.classifyTerminal(err)
	}
	return n, wrapNotFound(s.container.classify(s.ctx, err))
}

// Done and TerminalError preserve the underlying CLI stream's completion
// metadata through the classification wrapper.
func (s *classifyingStream) Done() <-chan struct{} {
	status, ok := s.ReadCloser.(interface{ Done() <-chan struct{} })
	if !ok {
		return nil
	}
	return status.Done()
}

func (s *classifyingStream) TerminalError() error {
	status, ok := s.ReadCloser.(interface{ TerminalError() error })
	if !ok {
		return nil
	}
	return s.classifyTerminal(status.TerminalError())
}

func (s *classifyingStream) classifyTerminal(err error) error {
	s.terminalOnce.Do(func() {
		if err == nil || errors.Is(err, io.EOF) {
			s.terminalErr = err
			return
		}
		s.terminalErr = wrapNotFound(s.container.classify(s.ctx, err))
	})
	return s.terminalErr
}
