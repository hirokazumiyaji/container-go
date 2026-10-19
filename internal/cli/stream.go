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

type SnapshotStreamer interface {
	StreamSnapshot(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// Stream starts a long-lived CLI invocation. Startup errors are returned
// directly; after a stream is returned, terminal process errors are
// delivered by Read.
func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	return r.stream(ctx, streamHooks{}, args...)
}

func (r *ExecRunner) StreamSnapshot(ctx context.Context, args ...string) (io.ReadCloser, error) {
	return r.stream(ctx, streamHooks{}, args...)
}

type streamHooks struct {
	// start is a test seam around exec.Cmd.Start. Production leaves it nil.
	start func(*exec.Cmd) error
	// beforeAttach runs after Start but before platform tree attachment.
	// Windows tests use it to characterize the post-Start attachment window.
	beforeAttach func(*processStream)
	// beforePublish runs after the process exists but before the lifecycle
	// barrier is opened. It lets tests cancel during that publication gap.
	beforePublish func(*processStream)
	// afterStart runs after publication. Main-era tests use it to cancel
	// once the child is owned.
	afterStart     func(*processStream)
	cancelObserved func()
	terminate      func(*exec.Cmd) error
}

func (r *ExecRunner) stream(ctx context.Context, hooks streamHooks, args ...string) (io.ReadCloser, error) {
	bin := r.binary()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	configureProcessTree(cmd)

	// Give the child one ordered OS endpoint for both stdout and stderr.
	// Separate pipes would be drained by independent goroutines, which can
	// publish stderr before earlier stdout even when the child wrote in the
	// opposite order. os/exec recognizes the identical *os.File and passes
	// the same open file description for fd 1 and fd 2, preserving the
	// kernel's write order. The single pump keeps a bounded diagnostic tail
	// while forwarding the merged child output.
	childRead, childWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	pr, pw := io.Pipe()
	lifecycle := newCommandLifecycle(ctx, cmd)
	stream := &processStream{
		ReadCloser: pr,
		cmd:        cmd,
		lifecycle:  lifecycle,
		ctx:        ctx,
		binary:     bin,
		identity:   r.identity(),
		args:       append([]string(nil), args...),
		output:     pw,
		stderr:     &tailBuffer{},
		// Keep both names as aliases for package-local compatibility;
		// they intentionally refer to the same ordered endpoint.
		stdoutRead:     childRead,
		stderrRead:     childRead,
		startDone:      lifecycle.startDone,
		waitDone:       lifecycle.waitDone,
		pumpsDone:      make(chan struct{}),
		cancelObserved: hooks.cancelObserved,
	}
	stream.ordered = newStreamOutput(pw, stream.stderr, &stream.terminalDrain)
	cmd.Stdout = childWrite
	cmd.Stderr = childWrite
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
		_ = childWrite.Close()
		_ = pr.Close()
		_ = pw.Close()
		stream.closeSourceFiles()
		if PermanentStartError(err) {
			return nil, fmt.Errorf("%s %s: %w: %w", bin, strings.Join(args, " "), ErrStreamSetup, err)
		}
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}

	// The child owns the inherited write end. Close the parent's copy
	// explicitly; it remains local to this function and is deliberately not
	// stored in processStream, so cancellation cannot race its ownership.
	_ = childWrite.Close()

	if hooks.beforeAttach != nil {
		hooks.beforeAttach(stream)
	}

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
	stream.stateMu.Lock()
	stream.started = true
	stream.stateMu.Unlock()

	// The hook is nil in production. It gives package tests a precise
	// cancellation-during-start interleaving without changing the public
	// API or relying on scheduler timing.
	if hooks.afterStart != nil {
		hooks.afterStart(stream)
	}

	stream.pumpWG.Add(2)
	go stream.pump(childRead, stream.ordered)
	go func() {
		stream.ordered.run()
		stream.pumpWG.Done()
	}()
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
	// identity names the backend CLI for error classification; see
	// ExecRunner.Identity.
	identity string
	args     []string
	output   *io.PipeWriter
	stderr   *tailBuffer
	ordered  *streamOutput

	// stdoutRead and stderrRead are aliases for the single ordered child
	// endpoint. The matching write end is local to Stream and is never
	// stored here, keeping endpoint ownership immutable across cancellation.
	stdoutRead *os.File
	stderrRead *os.File

	// startDone is closed after a successful Start and state publication.
	// It prevents a context callback from inspecting a half-started Cmd.
	// Production aliases the lifecycle barrier; unit tests may set it.
	startDone chan struct{}

	waitOnce     sync.Once
	waitDone     chan struct{}
	waitErr      error
	terminalOnce sync.Once
	terminalErr  error
	treeAttached bool
	// waitCalls is an internal invariant probe: the sole cmd.Wait call
	// must remain exactly-once even when lifecycle paths race.
	waitCalls atomic.Int32

	pumpWG          sync.WaitGroup
	pumpsDone       chan struct{}
	outputCloseOnce sync.Once
	sourceCloseOnce sync.Once
	readerCloseOnce sync.Once
	drainStartOnce  sync.Once
	drainDoneOnce   sync.Once
	terminalDrain   atomic.Bool
	drainCompleted  atomic.Bool

	cancelObserved     func()
	cancelObservedOnce sync.Once

	terminateOnce sync.Once
	terminateErr  error
	terminateTree func(*exec.Cmd) terminationResult
	closeTree     func()
	// terminationSignaled records positive evidence that the cancellation
	// callback actually signaled the child. A callback can run after the
	// child has already settled, so cancellation alone is not enough to
	// replace its process result with a context error.
	terminationSignaled bool
	// syntheticTermination is retained for platform-specific callers and
	// older package tests. It is a positive signal marker for APIs such as
	// Windows TerminateProcess, but is not required for the general
	// cancellation decision.
	syntheticTermination bool

	closeOnce sync.Once
	stateMu   sync.Mutex
	started   bool
	reaped    bool
	closed    bool
	cancelled bool
	ctxErr    error
}

// streamOutput retains a bounded diagnostic tail while forwarding the
// ordered child stream. The child-facing pipe merges stdout and stderr so
// chronology is preserved; the tail is intentionally retained for the
// terminal CLI diagnostic even though its source is the merged stream. A
// bounded queue decouples child draining from public-reader backpressure, so
// a terminal child can exit and release its stderr even when nobody is
// currently reading the public stream. The queue applies backpressure rather
// than discarding chunks, so a reader that is merely slow still observes
// every byte the child wrote.
type streamOutput struct {
	output        io.Writer
	stderr        *tailBuffer
	terminalDrain *atomic.Bool
	chunks        chan []byte
	workerDone    chan struct{}
	closeInput    sync.Once
	// drainC is closed by beginDrain, releasing a pump that is blocked on a
	// full queue once the terminal drain takes over the diagnostic tail.
	drainC    chan struct{}
	drainOnce sync.Once
}

const orderedOutputQueue = 32

func newStreamOutput(
	output io.Writer,
	stderr *tailBuffer,
	terminalDrain *atomic.Bool,
) *streamOutput {
	return &streamOutput{
		output:        output,
		stderr:        stderr,
		terminalDrain: terminalDrain,
		chunks:        make(chan []byte, orderedOutputQueue),
		workerDone:    make(chan struct{}),
		drainC:        make(chan struct{}),
	}
}

func (w *streamOutput) beginDrain() {
	w.drainOnce.Do(func() { close(w.drainC) })
}

func (w *streamOutput) Write(p []byte) (int, error) {
	if _, err := w.stderr.Write(p); err != nil {
		return 0, err
	}
	if w.terminalDrain.Load() {
		// The terminal process has already exited. The public reader is
		// no longer part of the diagnostic path, so keep consuming the
		// child endpoint without adding more queued output.
		return len(p), nil
	}
	chunk := append([]byte(nil), p...)
	select {
	case <-w.workerDone:
		// The public side has been closed. The tail above remains useful
		// for TerminalError, and the child must not be held hostage by a
		// blocked public reader.
		return len(p), nil
	case <-w.drainC:
		// The terminal drain owns the diagnostic tail from here on.
		return len(p), nil
	case w.chunks <- chunk:
		// A full queue applies ordinary backpressure. Discarding here would
		// silently drop child output from a reader that is only temporarily
		// slow, and the tail above is already complete.
		return len(p), nil
	}
}

func (w *streamOutput) run() {
	defer close(w.workerDone)
	for chunk := range w.chunks {
		if _, err := w.output.Write(chunk); err != nil {
			// Drain/Close deliberately close the public pipe. The child
			// reader will continue filling the bounded diagnostic tail.
			return
		}
	}
}

func (w *streamOutput) close() {
	w.closeInput.Do(func() { close(w.chunks) })
}

func (s *processStream) pump(r *os.File, output io.Writer) {
	defer s.pumpWG.Done()
	defer func() { _ = r.Close() }()
	_, _ = io.Copy(output, r)
	if closer, ok := output.(interface{ close() }); ok {
		closer.close()
	}
}

func (s *processStream) wait() {
	s.waitOnce.Do(func() {
		s.waitCalls.Add(1)
		if s.lifecycle != nil {
			s.lifecycle.wait()
			return
		}
		err := s.cmd.Wait()
		ctxErr := s.ctx.Err()
		s.stateMu.Lock()
		s.waitErr = err
		s.ctxErr = ctxErr
		// Close the platform ownership handle while holding stateMu. A
		// termination request holds the same lock through its signaling
		// decision, so the handle cannot be closed underneath it.
		if s.closeTree != nil {
			s.closeTree()
		}
		// Cmd.Wait has returned, so this process is no longer ours to
		// signal. In particular, never use its PID/PGID after this point:
		// the kernel may immediately reuse either identifier.
		s.reaped = true
		s.stateMu.Unlock()
		close(s.waitDone)
	})
	<-s.waitDone
}

func (s *processStream) cancel() error {
	err := s.requestTermination(true)
	s.stateMu.Lock()
	signaled := s.terminationSignaled || s.syntheticTermination
	terminationErr := s.terminateErr
	s.stateMu.Unlock()
	if !signaled && terminationErr != nil {
		// os/exec treats a non-nil Cancel error as an injected failure.
		// Keep the barrier/ownership cause on the stream state, but report
		// ErrProcessDone so a child that already settled is not replaced
		// by a synthetic cancellation error.
		return os.ErrProcessDone
	}
	return err
}

func (s *processStream) requestTermination(cancelled bool) error {
	// Preserve a terminal CLI failure that completed just before Close or
	// cancellation marks the stream as intentionally closed.
	s.captureTerminalError()

	if s.cancelObserved != nil {
		s.cancelObservedOnce.Do(s.cancelObserved)
	}

	if s.lifecycle != nil {
		return s.requestLifecycleTermination(cancelled)
	}
	return s.requestDirectTermination(cancelled)
}

func (s *processStream) requestLifecycleTermination(cancelled bool) error {
	err := s.lifecycle.terminate(cancelled)
	result := s.lifecycle.lastTermination()
	s.stateMu.Lock()
	if cancelled {
		s.cancelled = true
	} else {
		s.closed = true
	}
	s.terminateErr = err
	if result.active {
		s.terminationSignaled = true
	}
	if cancelled && result.syntheticExit {
		s.syntheticTermination = true
	}
	s.stateMu.Unlock()

	// Terminate the owned process tree first so an output-heavy descendant
	// cannot keep a CLI child alive. closeSourceFiles then releases pumps
	// that may be blocked writing to the public reader.
	s.closeReader()
	s.closeSourceFiles()
	return err
}

func (s *processStream) requestDirectTermination(cancelled bool) error {
	// Start owns cmd.Process until it has published a successful start.
	// Waiting on the barrier also makes cancellation before Start returns
	// safe without reading a concurrently initialized exec.Cmd.
	if s.startDone != nil {
		<-s.startDone
	}
	s.stateMu.Lock()
	if !s.started || s.reaped {
		s.stateMu.Unlock()
		s.closeReader()
		s.closeSourceFiles()
		return os.ErrProcessDone
	}
	if cancelled {
		s.cancelled = true
	} else {
		s.closed = true
	}

	// Serialize the ownership check with the tree signal. Once Wait marks
	// the child reaped, a delayed Close or context callback observes the
	// reaped flag and cannot signal a stale PID/PGID.
	s.terminateOnce.Do(func() {
		result := terminationResult{err: os.ErrProcessDone}
		if s.terminateTree != nil {
			result = s.terminateTree(s.cmd)
		}
		s.terminateErr = result.err
		if result.active {
			s.terminationSignaled = true
		}
		if cancelled && result.syntheticExit {
			s.syntheticTermination = true
		}
	})
	terminateErr := s.terminateErr
	s.stateMu.Unlock()

	s.closeReader()
	s.closeSourceFiles()
	return terminateErr
}

func (s *processStream) closeSourceFiles() {
	s.sourceCloseOnce.Do(func() {
		var previous *os.File
		for _, f := range []*os.File{s.stdoutRead, s.stderrRead} {
			if f == nil || f == previous {
				continue
			}
			_ = f.Close()
			previous = f
		}
	})
}

func (s *processStream) closeReader() {
	s.readerCloseOnce.Do(func() { _ = s.ReadCloser.Close() })
}

func (s *processStream) waitResult() error {
	s.wait()
	if s.lifecycle != nil {
		return s.lifecycle.error()
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.waitErr
}

const terminalDrainTimeout = 100 * time.Millisecond

// Drain completes the output pumps after the child has exited. It closes
// the public pipe to release a pump blocked on backpressure, then lets the
// stderr pump consume the remaining child output for its diagnostic tail.
// If the caller or the short safety deadline expires, source files are
// closed and Drain still waits for pumpsDone before returning. That wait is
// what makes a later TerminalError snapshot complete rather than a partial
// stderr tail.
func (s *processStream) Drain(ctx context.Context) error {
	drainCtx, cancel := context.WithTimeout(ctx, terminalDrainTimeout)
	defer cancel()

	s.drainStartOnce.Do(func() {
		s.terminalDrain.Store(true)
		if s.ordered != nil {
			s.ordered.beginDrain()
		}
		s.outputCloseOnce.Do(func() { _ = s.output.Close() })
	})
	complete := func() {
		s.drainDoneOnce.Do(func() { s.drainCompleted.Store(true) })
	}
	select {
	case <-s.pumpsDone:
		complete()
		return nil
	case <-drainCtx.Done():
		s.closeSourceFiles()
		<-s.pumpsDone
		complete()
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
}

// Done is closed after the child has been reaped. It is intentionally
// optional on the public Streamer interface so existing test and adapter
// runners remain source-compatible. TerminalError drains any output pumps
// that are still finishing before it snapshots stderr.
func (s *processStream) Done() <-chan struct{} { return s.waitDone }

// TerminalError reports and caches the process result. On a terminal failure
// it first finishes the output pumps so the diagnostic tail is complete;
// callers should treat the stream as terminal and close it afterward.
// The snapshot is deliberately separate from Read so a consumer that matched
// a log line can check whether the CLI had already failed before declaring
// readiness.
func (s *processStream) TerminalError() error {
	s.terminalOnce.Do(func() {
		s.terminalErr = s.terminalError(s.waitResult())
	})
	return s.terminalErr
}

func (s *processStream) captureTerminalError() {
	if s.lifecycle != nil && !s.lifecycle.isReaped() {
		return
	}
	if s.lifecycle == nil {
		s.stateMu.Lock()
		reaped := s.reaped
		s.stateMu.Unlock()
		if !reaped {
			return
		}
	}
	_ = s.TerminalError()
}

func (s *processStream) terminalError(waitErr error) error {
	s.stateMu.Lock()
	closed := s.closed
	cancelled := s.cancelled
	terminationSignaled := s.terminationSignaled || s.syntheticTermination
	ctxErr := s.ctxErr
	if s.lifecycle != nil && ctxErr == nil {
		ctxErr = s.lifecycle.contextError()
	}
	s.stateMu.Unlock()
	if closed {
		return io.EOF
	}
	var exitErr *exec.ExitError
	hasExit := errors.As(waitErr, &exitErr)
	// A context callback is not itself evidence that the child was
	// terminated. Only a positive signal result may replace an already
	// settled process/CLI failure (or a successful exit) with the context
	// cause. This matters when cancellation races a process that has
	// already exited, or when the platform kill operation reports
	// os.ErrProcessDone.
	if cancelled && terminationSignaled {
		return s.contextError()
	}

	if waitErr == nil {
		if cancelled {
			// The process settled successfully after the callback ran but
			// before it could signal the child. Preserve that result.
			return nil
		}
		if ctxErr != nil || s.ctx.Err() != nil {
			return s.contextError()
		}
		return nil
	}

	// cmd.Wait only proves that the direct child was reaped. A pump can
	// still be blocked trying to hand a chunk to the public reader, so
	// finish the drain before constructing the diagnostic error. A context
	// race does not discard the already-settled process result.
	var drainErr error
	if s.pumpsDone != nil && !s.drainCompleted.Load() {
		drainErr = s.Drain(s.ctx)
	}
	var terminalErr error
	if hasExit {
		terminalErr = s.cliError(exitErr)
	} else {
		terminalErr = fmt.Errorf("%s %s: %w", s.binary, strings.Join(s.args, " "), waitErr)
	}
	if ctxErr != nil || s.ctx.Err() != nil {
		// Keep the settled process error matchable (especially *CLIError)
		// while still exposing the caller's cancellation cause. The
		// cancellation callback alone never discards terminalErr.
		return errors.Join(terminalErr, s.contextError())
	}
	if drainErr != nil {
		return errors.Join(terminalErr, fmt.Errorf("%s %s: stream drain: %w", s.binary, strings.Join(s.args, " "), drainErr))
	}
	return terminalErr
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
	if waitErr != nil {
		return s.TerminalError()
	}

	s.stateMu.Lock()
	closed := s.closed
	cancelled := s.cancelled
	terminationSignaled := s.terminationSignaled || s.syntheticTermination
	ctxErr := s.ctxErr
	if s.lifecycle != nil && ctxErr == nil {
		ctxErr = s.lifecycle.contextError()
	}
	s.stateMu.Unlock()
	if (cancelled && terminationSignaled) || (!cancelled && (ctxErr != nil || s.ctx.Err() != nil)) {
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
		Binary:   s.identity,
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
		if s.pumpsDone != nil {
			<-s.pumpsDone
		}
	})
	return nil
}
