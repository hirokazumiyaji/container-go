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
	var reader io.ReadCloser
	err := c.withVerifiedOperationTarget(qCtx, false, func(target string, _ *engineInfo) error {
		args := c.eng.logsArgs(target, false)
		if extra := opts.args(); len(extra) > 0 {
			// Insert --tail/--since before the container ID (last arg).
			args = append(args[:len(args)-1], append(extra, args[len(args)-1])...)
		}
		stdout, stderr, err := c.runner.Run(qCtx, args...)
		if err != nil {
			return wrapNotFoundFor(c.eng, c.classify(ctx, err))
		}
		// docker logs splits the container's streams across the CLI's
		// stdout and stderr; a snapshot carries both.
		reader = io.NopCloser(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

// FollowLogs streams the container's log output until Close is called
// or the context is cancelled. Close terminates the underlying CLI
// process.
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	s, ok := c.runner.(cli.Streamer)
	if !ok {
		return nil, errors.New("logs: runner does not support streaming")
	}
	target, _, release, err := c.acquireVerifiedOperationTarget(ctx, false)
	if err != nil {
		return nil, err
	}
	stream, err := s.Stream(ctx, c.eng.logsArgs(target, true)...)
	if err != nil {
		release()
		return nil, wrapNotFoundFor(c.eng, c.classify(ctx, err))
	}
	classified := &classifyingStream{
		ReadCloser: stream,
		container:  c,
		ctx:        ctx,
		release:    release,
		done:       make(chan struct{}),
		closeDone:  make(chan struct{}),
	}
	var processDone <-chan struct{}
	if status, ok := stream.(interface{ Done() <-chan struct{} }); ok {
		processDone = status.Done()
	}
	go classified.watchContext(processDone)
	return classified, nil
}

type terminalStreamStatus interface {
	TerminalError() error
}

type classifyingStream struct {
	io.ReadCloser
	ctx         context.Context
	container   *Container
	once        sync.Once
	release     func()
	releaseOnce sync.Once
	terminal    error
	done        chan struct{}
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error
}

func (s *classifyingStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err == nil {
		return n, nil
	}
	if terminal := s.TerminalError(); terminal != nil {
		s.finish()
		return n, terminal
	}
	if errors.Is(err, io.EOF) {
		s.finish()
		return n, err
	}
	s.finish()
	return n, s.classifyTerminal(err)
}

func (s *classifyingStream) watchContext(processDone <-chan struct{}) {
	select {
	case <-s.ctx.Done():
		_ = s.Close()
	case <-processDone:
		s.finish()
	case <-s.done:
	}
}

func (s *classifyingStream) finish() {
	s.releaseOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
		if s.release != nil {
			s.release()
		}
	})
}

func (s *classifyingStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.ReadCloser.Close()
		close(s.closeDone)
		s.finish()
	})
	<-s.closeDone
	return s.closeErr
}

func (s *classifyingStream) TerminalError() error {
	s.once.Do(func() {
		if status, ok := s.ReadCloser.(terminalStreamStatus); ok {
			if terminal := status.TerminalError(); terminal != nil {
				s.terminal = s.classifyTerminal(terminal)
			}
		}
	})
	return s.terminal
}

func (s *classifyingStream) classifyTerminal(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	classified := s.container.classify(s.ctx, err)
	if classified == nil {
		return err
	}
	if !errors.Is(classified, err) && !errors.Is(err, classified) {
		classified = errors.Join(classified, err)
	}
	return wrapNotFoundFor(s.container.eng, classified)
}
