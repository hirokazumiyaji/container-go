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
	"syscall"
	"time"
)

// Streamer starts a long-lived CLI invocation (e.g. `logs --follow`)
// and exposes its combined stdout and stderr as a stream. A terminal
// process failure is returned by Read. Closing the stream terminates the
// child process.
type Streamer interface {
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	// Keep the reader open until the command has been waited. The exec
	// package's copy goroutines can therefore finish writing all output
	// before the reader observes EOF.
	pr, pw := io.Pipe()
	stderr := &limitedBuffer{}
	cmd.Stdout = pw
	cmd.Stderr = &streamStderr{output: pw, stderr: stderr}
	stream := &processStream{
		ReadCloser: pr,
		cmd:        cmd,
		ctx:        ctx,
		binary:     bin,
		args:       append([]string(nil), args...),
		output:     pw,
		stderr:     stderr,
		waitDone:   make(chan struct{}),
	}
	// Cancel closes the reader as well as killing the child. This is the
	// callback used by exec.CommandContext's context watcher.
	cmd.Cancel = stream.cancel
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		if permanentStreamStartError(bin, err) {
			return nil, fmt.Errorf("%s %s: %w: %w", bin, strings.Join(args, " "), ErrStreamSetup, err)
		}
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}

	// Wait is deliberately owned by one caller. Every other lifecycle
	// path coordinates through waitOnce, so EOF, cancellation, and Close
	// cannot reap the child more than once.
	go stream.wait()
	return stream, nil
}

// permanentStreamStartError reports setup failures that cannot change when
// the same runner opens the same backend again. Explicit relative paths
// bypass exec.LookPath just like absolute paths, so their permission,
// existence, and executable-format failures need explicit classification.
func permanentStreamStartError(_ string, err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true
	}
	return os.IsPermission(err) || os.IsNotExist(err) || errors.Is(err, syscall.ENOEXEC)
}

type processStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	ctx    context.Context
	binary string
	args   []string
	output *io.PipeWriter
	stderr *limitedBuffer

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error

	closeOnce       sync.Once
	readerCloseOnce sync.Once
	stateMu         sync.Mutex
	closed          bool
	cancelled       bool
	completed       bool
	ctxErr          error
}

// streamStderr forwards CLI stderr to the stream while retaining a
// bounded diagnostic copy for a terminal CLIError.
type streamStderr struct {
	output io.Writer
	stderr *limitedBuffer
}

func (w *streamStderr) Write(p []byte) (int, error) {
	if _, err := w.stderr.Write(p); err != nil {
		return 0, err
	}
	return w.output.Write(p)
}

// limitedBuffer retains at most maxStderr bytes while always reporting
// a complete write to the forwarding stream.
type limitedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) < maxStderr {
		remaining := maxStderr - len(b.data)
		if remaining > len(p) {
			remaining = len(p)
		}
		b.data = append(b.data, p[:remaining]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

func (s *processStream) wait() {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		ctxErr := s.ctx.Err()
		s.stateMu.Lock()
		s.waitErr = err
		s.ctxErr = ctxErr
		s.completed = true
		s.stateMu.Unlock()
		// Cmd.Wait has joined the stdout/stderr copy goroutines, so no
		// output can be lost by closing the writer now.
		_ = s.output.Close()
		close(s.waitDone)
	})
}

func (s *processStream) cancel() error {
	return s.requestTermination(true)
}

func (s *processStream) requestTermination(cancelled bool) error {
	s.stateMu.Lock()
	if s.completed {
		s.stateMu.Unlock()
		s.closeReader()
		return os.ErrProcessDone
	}
	if cancelled {
		s.cancelled = true
	} else {
		s.closed = true
	}
	s.stateMu.Unlock()

	// Kill first so an output-heavy child cannot turn the intentional
	// termination into a SIGPIPE exit. Closing the reader then releases
	// any exec copy goroutine blocked while forwarding output.
	killErr := s.cmd.Process.Kill()
	s.closeReader()
	return killErr
}

func (s *processStream) closeReader() {
	s.readerCloseOnce.Do(func() { _ = s.ReadCloser.Close() })
}

func (s *processStream) waitResult() error {
	s.wait()
	<-s.waitDone
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.waitErr
}

// Done reports completion of the child process. It lets a consumer that
// found a readiness marker perform a short settle check before accepting a
// stream that may have exited unsuccessfully immediately afterward.
func (s *processStream) Done() <-chan struct{} {
	return s.waitDone
}

// TerminalError returns the process error after Done has closed. It mirrors
// the error mapping used by Read and is safe to call more than once.
func (s *processStream) TerminalError() error {
	waitErr := s.waitResult()
	s.stateMu.Lock()
	closed := s.closed
	cancelled := s.cancelled
	ctxErr := s.ctxErr
	s.stateMu.Unlock()
	if waitErr == nil {
		if cancelled || ctxErr != nil {
			return s.contextError()
		}
		if closed {
			return io.EOF
		}
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if cancelled {
			return s.contextError()
		}
		if closed {
			return io.EOF
		}
		if exitErr.ExitCode() >= 0 || ctxErr == nil {
			return s.cliError(exitErr)
		}
		return s.contextError()
	}
	if cancelled || ctxErr != nil {
		return s.contextError()
	}
	if closed {
		return io.EOF
	}
	return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), waitErr)
}

func (s *processStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, readErr := s.ReadCloser.Read(p)
	if n > 0 {
		if errors.Is(readErr, io.EOF) {
			return n, s.readError(readErr)
		}
		return n, readErr
	}
	if readErr == nil {
		return 0, nil
	}
	return 0, s.readError(readErr)
}

func (s *processStream) readError(readErr error) error {
	waitErr := s.waitResult()
	s.stateMu.Lock()
	closed := s.closed
	cancelled := s.cancelled
	ctxErr := s.ctxErr
	s.stateMu.Unlock()

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			// Cancellation and Close are intentional terminal paths. A
			// child can report SIGPIPE (or another numeric code) when it
			// races with the reader shutdown, so those paths take
			// precedence over the exit status.
			if cancelled {
				return s.contextError()
			}
			if closed {
				return io.EOF
			}
			if exitErr.ExitCode() >= 0 {
				return s.cliError(exitErr)
			}
			// A signal not caused by this stream is still a terminal CLI
			// failure. If the context was done while Wait returned, the
			// command was interrupted by its context instead.
			if ctxErr != nil {
				return s.contextError()
			}
			return s.cliError(exitErr)
		}
		if cancelled || ctxErr != nil {
			return s.contextError()
		}
		if closed {
			return io.EOF
		}
		return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), waitErr)
	}

	if cancelled || ctxErr != nil {
		return s.contextError()
	}
	if closed {
		return io.EOF
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	return io.EOF
}

func (s *processStream) cliError(exitErr *exec.ExitError) error {
	return &CLIError{
		Binary:   s.binary,
		Args:     s.args,
		ExitCode: exitErr.ExitCode(),
		Stderr:   truncateStderr(s.stderr.String()),
	}
}

func (s *processStream) contextError() error {
	err := s.ctx.Err()
	if err == nil {
		err = context.Canceled
	}
	return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), err)
}

func (s *processStream) Close() error {
	s.closeOnce.Do(func() {
		_ = s.requestTermination(false)
		s.wait()
	})
	return nil
}
