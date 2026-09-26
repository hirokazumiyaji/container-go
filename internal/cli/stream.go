package cli

import (
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

	// Use independent OS pipes for stdout and stderr. os/exec does not
	// create copy goroutines for caller-owned *os.File values, so Wait can
	// reap the child even when the public stream is not being read. The
	// pumps below are the only components subject to public backpressure.
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	publicRead, publicWrite := io.Pipe()
	stream := &processStream{
		ReadCloser: publicRead,
		cmd:        cmd,
		ctx:        ctx,
		binary:     bin,
		args:       append([]string(nil), args...),
		output:     publicWrite,
		stderr:     &tailBuffer{},
		stdoutRead: stdoutRead,
		stderrRead: stderrRead,
		waitDone:   make(chan struct{}),
		pumpsDone:  make(chan struct{}),
	}
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite
	// Cancellation must also release the public reader; otherwise a
	// caller blocked in Read can keep a cancelled process's output pump
	// (and its name lock) alive indefinitely.
	cmd.Cancel = func() error {
		stream.cancelProcess()
		return nil
	}
	if err := cmd.Start(); err != nil {
		_ = publicRead.Close()
		_ = publicWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		_ = stderrRead.Close()
		_ = stderrWrite.Close()
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	// The child owns duplicate write descriptors after Start. Closing the
	// parent's copies lets the read pumps observe EOF when it exits.
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()

	stream.startPumps()
	go stream.wait()
	return stream, nil
}

type processStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	ctx    context.Context
	binary string
	args   []string

	output     *io.PipeWriter
	stdoutRead *os.File
	stderrRead *os.File
	waitDone   chan struct{}
	pumpsDone  chan struct{}
	pumpWG     sync.WaitGroup

	waitOnce   sync.Once
	closeOnce  sync.Once
	sourceOnce sync.Once
	mu         sync.Mutex
	waitErr    error
	closed     bool
	cancelled  bool
	stderr     *tailBuffer
}

func (s *processStream) startPumps() {
	s.pumpWG.Add(2)
	go s.pump(s.stdoutRead, false)
	go s.pump(s.stderrRead, true)
	go func() {
		s.pumpWG.Wait()
		_ = s.output.Close()
		close(s.pumpsDone)
	}()
}

func (s *processStream) pump(source *os.File, isStderr bool) {
	defer s.pumpWG.Done()
	defer func() { _ = source.Close() }()
	buf := make([]byte, 32*1024)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			// Capture diagnostics before applying public backpressure. A
			// terminal error remains available even when the caller never
			// drains the merged stream.
			if isStderr {
				_, _ = s.stderr.Write(chunk)
			}
			if _, writeErr := s.output.Write(chunk); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *processStream) wait() {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		s.mu.Lock()
		s.waitErr = err
		s.mu.Unlock()
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
	cancelled := s.cancelled
	s.mu.Unlock()
	if closed {
		return nil
	}
	if ctxErr := s.ctx.Err(); ctxErr != nil || cancelled {
		if ctxErr == nil {
			ctxErr = context.Canceled
		}
		return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), ctxErr)
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

func (s *processStream) closeSources() {
	s.sourceOnce.Do(func() {
		_ = s.stdoutRead.Close()
		_ = s.stderrRead.Close()
		_ = s.ReadCloser.Close()
		_ = s.output.Close()
	})
}

func (s *processStream) cancelProcess() {
	s.mu.Lock()
	if s.closed || s.cancelled {
		s.mu.Unlock()
		return
	}
	s.cancelled = true
	process := s.cmd.Process
	s.mu.Unlock()
	if process != nil {
		_ = process.Kill()
	}
	s.closeSources()
}

func (s *processStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		process := s.cmd.Process
		s.mu.Unlock()
		if process != nil {
			_ = process.Kill()
		}
		s.closeSources()
		s.wait()
		<-s.pumpsDone
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
