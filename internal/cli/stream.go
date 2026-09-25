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
// process failure is returned by Read after Stream has returned. Closing
// the stream terminates the direct CLI child. On platforms with process
// groups it also makes a best-effort attempt to terminate descendants
// while that child is owned; this package does not reap those descendants.
// Once the direct child is reaped, Close does not signal its former group.
type Streamer interface {
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// Stream starts a long-lived CLI invocation. Startup errors are returned
// directly; after a stream is returned, terminal process errors are
// delivered by Read.
func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	return r.stream(ctx, streamHooks{}, args...)
}

type streamHooks struct {
	// start is a test seam around exec.Cmd.Start. Production leaves it nil.
	start func(*exec.Cmd) error
	// beforePublish runs after the process exists but before the lifecycle
	// barrier is opened. It lets tests cancel during that publication gap.
	beforePublish  func(*processStream)
	cancelObserved func()
	terminate      func(*exec.Cmd) error
	// afterStart is retained for older package-local tests. It runs after
	// the lifecycle has been published.
	afterStart func(*processStream)
}

func (r *ExecRunner) stream(ctx context.Context, hooks streamHooks, args ...string) (io.ReadCloser, error) {
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
	lifecycle := newCommandLifecycle(ctx, cmd)
	stream := &processStream{
		ReadCloser:     pr,
		cmd:            cmd,
		lifecycle:      lifecycle,
		ctx:            ctx,
		binary:         bin,
		args:           append([]string(nil), args...),
		output:         pw,
		stderr:         &tailBuffer{},
		stdoutRead:     stdoutRead,
		stderrRead:     stderrRead,
		stderrDone:     make(chan struct{}),
		startDone:      lifecycle.startDone,
		waitDone:       lifecycle.waitDone,
		pumpsDone:      make(chan struct{}),
		cancelObserved: hooks.cancelObserved,
	}
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite
	// Cancel closes the reader as well as terminating the owned process
	// tree. The lifecycle, rather than this callback, owns Wait.
	cmd.Cancel = stream.cancel

	start := hooks.start
	if start == nil {
		start = func(command *exec.Cmd) error { return command.Start() }
	}
	if err := start(cmd); err != nil {
		// Start does not return a usable process on failure. Close the
		// ownership barrier anyway so no lifecycle caller can wait forever.
		lifecycle.failStart()
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
		_ = pr.Close()
		_ = pw.Close()
		stream.closeSourceFiles()
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}

	// The child owns inherited copies of the write ends. Close the
	// parent copies explicitly; they remain local to this function and are
	// deliberately not stored in processStream, so cancellation can never
	// race ownership of them.
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()

	var tree processTree
	if hooks.terminate != nil {
		tree = processTreeFunc(hooks.terminate)
	} else {
		tree, err = newProcessTree(cmd)
		if err != nil {
			// A platform tree is an enhancement. The direct process handle
			// remains a safe cancellation path if attachment is unavailable.
			tree = directProcessTree{}
		} else {
			stream.treeAttached = true
		}
	}

	// The context watcher can run as soon as Start returns. Keep the
	// ownership barrier closed until all process handles are published.
	if hooks.beforePublish != nil {
		hooks.beforePublish(stream)
	}
	lifecycle.publishStart(tree)
	if hooks.afterStart != nil {
		hooks.afterStart(stream)
	}

	stream.pumpWG.Add(2)
	go stream.pump(stdoutRead, false)
	go stream.pump(stderrRead, true)
	go func() {
		stream.pumpWG.Wait()
		stream.outputCloseOnce.Do(func() { _ = stream.output.Close() })
		close(stream.pumpsDone)
	}()

	// Wait is deliberately owned by one lifecycle caller. Every lifecycle
	// path coordinates through that owner, so EOF, cancellation, and Close
	// cannot reap the child more than once.
	go stream.wait()
	return stream, nil
}

type processStream struct {
	io.ReadCloser
	cmd       *exec.Cmd
	lifecycle *commandLifecycle
	ctx       context.Context
	binary    string
	args      []string
	output    *io.PipeWriter
	stderr    *tailBuffer

	// stdoutRead and stderrRead belong to the stream after construction.
	// The matching write ends are local to Stream and are never stored
	// here; this keeps endpoint ownership immutable across cancellation.
	stdoutRead *os.File
	stderrRead *os.File
	stderrDone chan struct{}

	// startDone is closed after a successful Start and all process handles
	// are published. It prevents a context callback from inspecting a
	// half-started Cmd.
	startDone chan struct{}

	waitOnce           sync.Once
	waitDone           chan struct{}
	waitCalls          atomic.Int32
	treeAttached       bool
	cancelObserved     func()
	cancelObservedOnce sync.Once

	pumpWG          sync.WaitGroup
	pumpsDone       chan struct{}
	outputCloseOnce sync.Once
	sourceCloseOnce sync.Once
	readerCloseOnce sync.Once

	closeOnce           sync.Once
	stateMu             sync.Mutex
	closed              bool
	cancelled           bool
	terminalSnapshot    error
	terminalSnapshotSet bool
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

// tailBuffer is a bounded rolling diagnostic buffer. It always reports a
// complete write, even when older bytes are discarded.
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

func (s *processStream) pump(r *os.File, stderr bool) {
	defer s.pumpWG.Done()
	defer func() { _ = r.Close() }()
	if stderr {
		defer close(s.stderrDone)
	}
	var output io.Writer = s.output
	if stderr {
		output = &streamOutput{output: s.output, stderr: s.stderr}
	}
	_, _ = io.Copy(output, r)
}

func (s *processStream) wait() {
	s.waitOnce.Do(func() {
		s.waitCalls.Add(1)
		s.lifecycle.wait()
	})
	<-s.waitDone
}

func (s *processStream) cancel() error {
	return s.requestTermination(true)
}

func (s *processStream) requestTermination(cancelled bool) error {
	// Preserve a terminal CLI failure that completed just before Close or
	// cancellation marks the stream as intentionally closed.
	s.captureTerminalError()

	s.stateMu.Lock()
	if cancelled {
		s.cancelled = true
	} else {
		s.closed = true
	}
	s.stateMu.Unlock()

	if s.cancelObserved != nil {
		s.cancelObservedOnce.Do(s.cancelObserved)
	}
	err := s.lifecycle.terminate(cancelled)
	// Kill the owned process tree first so an output-heavy descendant
	// cannot keep the CLI alive. closeSourceFiles then releases pumps that
	// may be blocked writing to the public reader.
	s.closeReader()
	s.closeSourceFiles()
	return err
}

func (s *processStream) closeSourceFiles() {
	s.sourceCloseOnce.Do(func() {
		for _, f := range []*os.File{s.stdoutRead, s.stderrRead} {
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
	return s.lifecycle.error()
}

// Done reports completion of the direct child. It is an optional status
// capability used by readiness strategies; it does not wait for descendants.
func (s *processStream) Done() <-chan struct{} {
	return s.waitDone
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
	return s.processError(s.waitResult(), readErr, false)
}

// TerminalError returns the process-level terminal error, if any. It is
// deliberately separate from Read so a consumer that matched a log line can
// check whether the CLI had already failed before declaring readiness.
func (s *processStream) TerminalError() error {
	s.stateMu.Lock()
	if s.terminalSnapshotSet {
		err := s.terminalSnapshot
		s.stateMu.Unlock()
		return err
	}
	s.stateMu.Unlock()

	waitErr := s.waitResult()
	// The stderr pump normally drains while a log scanner is active. Give
	// it a short bounded chance to record the terminal diagnostic before
	// constructing CLIError, without making a caller that is not reading
	// the stream wait forever on public-pipe backpressure.
	timer := time.NewTimer(10 * time.Millisecond)
	select {
	case <-s.stderrDone:
	case <-timer.C:
	}
	timer.Stop()
	err := s.processError(waitErr, io.EOF, true)
	s.stateMu.Lock()
	if !s.terminalSnapshotSet {
		s.terminalSnapshot = err
		s.terminalSnapshotSet = true
	}
	s.stateMu.Unlock()
	return err
}

func (s *processStream) captureTerminalError() {
	if !s.lifecycle.isReaped() {
		return
	}
	s.stateMu.Lock()
	if s.terminalSnapshotSet {
		s.stateMu.Unlock()
		return
	}
	s.stateMu.Unlock()
	_ = s.TerminalError()
}

func (s *processStream) processError(waitErr, readErr error, terminalOnly bool) error {
	s.stateMu.Lock()
	closed := s.closed
	cancelled := s.cancelled
	ctxErr := s.lifecycle.contextError()
	s.stateMu.Unlock()

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			// Cancellation and Close are intentional terminal paths. A
			// child can report SIGPIPE (or another numeric code) when it
			// races with the reader shutdown, so those paths take
			// precedence over the exit status.
			if cancelled {
				if exitErr.ExitCode() >= 0 {
					return errors.Join(s.cliError(exitErr), s.contextError())
				}
				return s.contextError()
			}
			if closed {
				if terminalOnly {
					return nil
				}
				return io.EOF
			}
			if exitErr.ExitCode() >= 0 {
				cliErr := s.cliError(exitErr)
				if ctxErr != nil {
					return errors.Join(cliErr, s.contextError())
				}
				return cliErr
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
			if terminalOnly {
				return nil
			}
			return io.EOF
		}
		return fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), waitErr)
	}

	if cancelled || ctxErr != nil {
		return s.contextError()
	}
	if closed {
		if terminalOnly {
			return nil
		}
		return io.EOF
	}
	if terminalOnly {
		return nil
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
