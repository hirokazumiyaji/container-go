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
	"sync/atomic"
	"time"
)

// Streamer starts a long-lived CLI invocation (e.g. `logs --follow`)
// and exposes its combined stdout and stderr as a stream. A terminal
// process failure is returned by Read. Closing the stream terminates the
// child process and its process group.
type Streamer interface {
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	configureProcessTree(cmd)

	// Keep OS pipes as the command's stdout/stderr. os/exec does not join
	// a caller-owned *os.File with a copy goroutine, so Wait can reap the
	// direct child even when the public stream is not being read. Our two
	// pumps merge those pipes into the public stream asynchronously.
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
	pr, pw := io.Pipe()
	stream := &processStream{
		ReadCloser:  pr,
		cmd:         cmd,
		ctx:         ctx,
		binary:      bin,
		args:        append([]string(nil), args...),
		output:      pw,
		stderr:      &tailBuffer{},
		stdoutRead:  stdoutRead,
		stderrRead:  stderrRead,
		stdoutWrite: stdoutWrite,
		stderrWrite: stderrWrite,
		waitDone:    make(chan struct{}),
		pumpsDone:   make(chan struct{}),
	}
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite
	// Cancel closes the reader as well as killing the process tree. This
	// is the callback used by exec.CommandContext's context watcher.
	cmd.Cancel = stream.cancel
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		stream.closeSourceFiles()
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}

	// The child owns inherited copies of the write ends. Closing the
	// parent's copies lets the pumps observe EOF when the child exits.
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	stream.stdoutWrite = nil
	stream.stderrWrite = nil

	stream.pumpWG.Add(2)
	go stream.pump(stdoutRead, false)
	go stream.pump(stderrRead, true)
	go func() {
		stream.pumpWG.Wait()
		stream.outputCloseOnce.Do(func() { _ = stream.output.Close() })
		close(stream.pumpsDone)
	}()

	// Wait is deliberately owned by one caller. Every lifecycle path
	// coordinates through waitOnce, so EOF, cancellation, and Close cannot
	// reap the child more than once.
	go stream.wait()
	return stream, nil
}

type processStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	ctx    context.Context
	binary string
	args   []string
	output *io.PipeWriter
	stderr *tailBuffer

	stdoutRead  *os.File
	stderrRead  *os.File
	stdoutWrite *os.File
	stderrWrite *os.File

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
	// waitCalls is an internal invariant probe: the sole cmd.Wait call
	// must remain exactly-once even when lifecycle paths race.
	waitCalls atomic.Int32

	pumpWG          sync.WaitGroup
	pumpsDone       chan struct{}
	outputCloseOnce sync.Once
	sourceCloseOnce sync.Once
	readerCloseOnce sync.Once

	terminateOnce sync.Once
	terminateErr  error

	closeOnce sync.Once
	stateMu   sync.Mutex
	closed    bool
	cancelled bool
	ctxErr    error
}

// streamOutput retains a rolling stderr diagnostic while forwarding the
// bytes to the public stream.
type streamOutput struct {
	output io.Writer
	stderr *tailBuffer
}

func (w *streamOutput) Write(p []byte) (int, error) {
	if _, err := w.stderr.Write(p); err != nil {
		return 0, err
	}
	return w.output.Write(p)
}

func (s *processStream) pump(r *os.File, stderr bool) {
	defer s.pumpWG.Done()
	defer func() { _ = r.Close() }()
	var output io.Writer = s.output
	if stderr {
		output = &streamOutput{output: s.output, stderr: s.stderr}
	}
	_, _ = io.Copy(output, r)
}

func (s *processStream) wait() {
	s.waitOnce.Do(func() {
		s.waitCalls.Add(1)
		err := s.cmd.Wait()
		ctxErr := s.ctx.Err()
		s.stateMu.Lock()
		s.waitErr = err
		s.ctxErr = ctxErr
		s.stateMu.Unlock()
		close(s.waitDone)
	})
}

func (s *processStream) cancel() error {
	return s.requestTermination(true)
}

func (s *processStream) requestTermination(cancelled bool) error {
	s.stateMu.Lock()
	if cancelled {
		s.cancelled = true
	} else {
		s.closed = true
	}
	s.stateMu.Unlock()

	// Kill the whole process group first so an output-heavy descendant
	// cannot keep the CLI alive. closeSourceFiles then releases pumps that
	// may be blocked writing to the public reader.
	s.terminateOnce.Do(func() {
		s.terminateErr = terminateProcessTree(s.cmd)
	})
	s.closeReader()
	s.closeSourceFiles()
	return s.terminateErr
}

func (s *processStream) closeSourceFiles() {
	s.sourceCloseOnce.Do(func() {
		for _, f := range []*os.File{s.stdoutRead, s.stderrRead, s.stdoutWrite, s.stderrWrite} {
			if f != nil {
				_ = f.Close()
			}
		}
	})
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
		Stderr:   s.stderr.String(),
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
		<-s.pumpsDone
	})
	return nil
}
