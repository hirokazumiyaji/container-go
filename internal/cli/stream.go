package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Streamer starts a long-lived CLI invocation (e.g. `logs --follow`)
// and exposes its combined stdout/stderr as a stream. A terminal
// non-zero process status is returned by Read after Stream returns.
type Streamer interface {
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	pr, pw := io.Pipe()
	stream := &processStream{
		ReadCloser: pr,
		cmd:        cmd,
		ctx:        ctx,
		binary:     bin,
		args:       append([]string(nil), args...),
		pipeWriter: pw,
		waitDone:   make(chan struct{}),
	}
	// Keep the two streams in the same public stream while retaining a
	// bounded stderr diagnostic for a terminal CLIError.
	cmd.Stdout = pw
	cmd.Stderr = io.MultiWriter(&stream.stderr, pw)
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	go stream.wait()
	return stream, nil
}

type processStream struct {
	io.ReadCloser
	cmd        *exec.Cmd
	ctx        context.Context
	binary     string
	args       []string
	pipeWriter *io.PipeWriter
	waitDone   chan struct{}

	waitOnce  sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	waitErr   error
	closed    bool
	stderr    tailBuffer
}

func (s *processStream) wait() {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		s.mu.Lock()
		s.waitErr = err
		s.mu.Unlock()
		_ = s.pipeWriter.Close()
		close(s.waitDone)
	})
	<-s.waitDone
}

func (s *processStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := s.ReadCloser.Read(p)
	if err == nil {
		return n, nil
	}
	if terminal := s.TerminalError(); terminal != nil {
		return n, terminal
	}
	return n, err
}

// TerminalError returns a terminal CLI error, if the process has ended
// with one. It deliberately returns nil for a clean EOF and for an
// intentional Close/context cancellation.
func (s *processStream) TerminalError() error {
	s.wait()
	s.mu.Lock()
	waitErr := s.waitErr
	closed := s.closed
	s.mu.Unlock()
	if closed || s.ctx.Err() != nil {
		if s.ctx.Err() != nil && !closed {
			return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), s.ctx.Err())
		}
		return nil
	}
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return &CLIError{
			Binary:   s.binary,
			Args:     s.args,
			ExitCode: exitErr.ExitCode(),
			Stderr:   s.stderr.String(),
		}
	}
	return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), waitErr)
}

func (s *processStream) Done() <-chan struct{} { return s.waitDone }

func (s *processStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.ReadCloser.Close()
		s.wait()
	})
	return nil
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= maxStderr {
		b.data = append(b.data[:0], p[len(p)-maxStderr:]...)
		return len(p), nil
	}
	if overflow := len(b.data) + len(p) - maxStderr; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}
