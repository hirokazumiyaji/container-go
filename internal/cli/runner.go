// Package cli executes a container backend CLI (`container` or `docker`)
// as a child process and classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Runner executes one backend CLI invocation.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}

// StdinRunner is the optional stdin extension of Runner. Its RunWithStdin
// follows the WriterRunner contract for output and additionally feeds stdin
// to the child. A runner that cannot own the child's standard input must not
// implement this interface: there is no safe fallback that leaves the
// command's stdin closed while the caller asked for input.
type StdinRunner interface {
	Runner

	RunWithStdin(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (OutputStats, error)
}

// RunWithStdin executes a CLI invocation through the optional StdinRunner
// extension. Unlike RunTo there is no legacy fallback: a runner without
// stdin support reports ErrStdinUnsupported, and the caller can decide
// whether that is a capability error.
func RunWithStdin(r Runner, ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (OutputStats, error) {
	sr, ok := r.(StdinRunner)
	if !ok {
		return OutputStats{}, fmt.Errorf("%w: runner %T cannot attach stdin", ErrStdinUnsupported, r)
	}
	return sr.RunWithStdin(ctx, stdin, stdout, stderr, args...)
}

// ExternalRunner identifies runners that execute the CLI as real child
// processes. container.Run registers containers started through such
// runners with the orphan-cleanup reaper, so a runner that wraps an
// ExecRunner forwards both methods to keep the production path intact
// under instrumentation. Test doubles that return canned results do
// not implement the interface.
type ExternalRunner interface {
	Runner

	// External reports whether the runner spawns real child processes.
	External() bool
	// ExternalBinary is the binary those child processes execute, or
	// "" when the runner defers to the engine's default.
	ExternalBinary() string
}

// External reports that ExecRunner spawns real child processes.
func (r *ExecRunner) External() bool { return true }

// ExternalBinary reports the binary ExecRunner spawns.
func (r *ExecRunner) ExternalBinary() string { return r.Binary }

// ExecRunner runs the CLI as a child process. Arguments are passed as an
// argv vector; no shell is involved. Cancellation terminates the lifecycle-
// owned local process tree; it does not claim to terminate a process inside
// a backend.
type ExecRunner struct {
	// Binary is the CLI executable. Empty means "container" resolved
	// from PATH.
	Binary string
	// Identity names the backend CLI this runner speaks for, which is what
	// error classification compares against. Empty means the executable names
	// it, which is right when Binary is the backend's own CLI. A caller that
	// points Binary at a wrapper sets this so the wrapper's errors are still
	// attributed to the backend that owns it.
	Identity string

	// Env is the optional slice of environment variables for child
	// processes. When non-empty, variables override or are added to the
	// process's environment.
	Env []string
}

func (r *ExecRunner) binary() string {
	if r.Binary == "" {
		return "container"
	}
	return r.Binary
}

// identity names the backend CLI for error classification.
func (r *ExecRunner) identity() string {
	if r.Identity != "" {
		return r.Identity
	}
	return r.binary()
}

// SetBinary overrides the CLI executable to run.
func (r *ExecRunner) SetBinary(binary string) {
	r.Binary = binary
}

// SetIdentity records which backend CLI this runner speaks for, for callers
// that wrap it and still want its errors attributed to the backend.
func (r *ExecRunner) SetIdentity(identity string) {
	r.Identity = identity
}

// ExternalEnv returns the runner's custom environment variables.
func (r *ExecRunner) ExternalEnv() []string {
	if len(r.Env) == 0 {
		return nil
	}
	return append([]string(nil), r.Env...)
}

type runHooks struct {
	start          func(*exec.Cmd) error
	beforePublish  func()
	cancelObserved func()
	terminate      func(*exec.Cmd) error
}

func (r *ExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return r.run(ctx, runHooks{}, args...)
}

func (r *ExecRunner) run(ctx context.Context, hooks runHooks, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	// Output buffers are returned whole: success output and non-zero
	// exec/log results must not be silently truncated. Only the
	// diagnostic copy inside CLIError is bounded.
	err := r.execute(ctx, hooks, nil, &stdout, &stderr, func() ([]byte, []byte) {
		return stdout.Bytes(), stderr.Bytes()
	}, args)
	return stdout.Bytes(), stderr.Bytes(), err
}

// RunTo runs the CLI while sending stdout and stderr directly to the
// supplied writers. The process is still fully drained after a sink
// fails or reaches its own limit, so a sink cannot block the child.
// A sink error is remembered rather than returned from the write itself;
// the final error contains it together with the process's terminal
// status. A CLIError diagnostic is taken from a bounded stderr tail, and
// its stdout side channel is empty because stdout is not retained.
func (r *ExecRunner) RunTo(ctx context.Context, stdout, stderr io.Writer, args ...string) (OutputStats, error) {
	return r.runTo(ctx, nil, stdout, stderr, args...)
}

// RunWithStdin is RunTo with stdin wired to the child process. A reader error
// is returned as *StdinReadError, including when the child exits successfully
// after receiving truncated input. When the command exits or the caller's
// context is cancelled, the child's input pipe and a closable reader are
// closed, and the copy goroutine is awaited for a bounded grace period. Go
// cannot interrupt a Read on a non-closable reader; such a copy may outlive
// the command until the caller's reader returns, and callers must provide
// input that reaches EOF or is closable.
func (r *ExecRunner) RunWithStdin(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (OutputStats, error) {
	return r.runTo(ctx, stdin, stdout, stderr, args...)
}

func (r *ExecRunner) runTo(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (OutputStats, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stdoutCount := &countingWriter{dst: stdout}
	stderrCount := &countingWriter{dst: stderr}
	diagnostic := newTailBuffer(maxStderrContext)
	runErr := r.execute(ctx, runHooks{}, stdin, stdoutCount, &diagnosticWriter{output: stderrCount, diagnostic: diagnostic}, func() ([]byte, []byte) {
		return nil, []byte(diagnostic.String())
	}, args)
	stdoutBytes, stdoutErr := stdoutCount.snapshot()
	stderrBytes, stderrErr := stderrCount.snapshot()
	stats := OutputStats{
		StdoutBytes:     stdoutBytes,
		StderrBytes:     stderrBytes,
		StdoutTruncated: writerTruncated(stdout),
		StderrTruncated: writerTruncated(stderr),
	}
	return stats, joinErrors(
		runErr,
		outputDeliveryError("stdout", stdoutErr),
		outputDeliveryError("stderr", stderrErr),
	)
}

// execute owns one command lifecycle. stdin, when non-nil, is copied to the
// child's standard input by a feed the lifecycle can stop. stdoutW and
// stderrW receive the child's streams; captured returns the stdout and
// stderr bytes available for the CLIError diagnostic once the child has
// been reaped.
func (r *ExecRunner) execute(ctx context.Context, hooks runHooks, stdin io.Reader, stdoutW, stderrW io.Writer, captured func() ([]byte, []byte), args []string) error {
	bin := r.binary()
	identity := r.identity()
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(r.Env) > 0 {
		cmd.Env = MergeEnv(os.Environ(), r.Env)
	}
	configureProcessTree(cmd)
	lifecycle := newCommandLifecycle(ctx, cmd)
	var feed *stdinFeed
	if stdin != nil {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			lifecycle.failStart()
			return markRunStatus(fmt.Errorf("%s: %w", commandText(bin, args), err), lifecycle.status())
		}
		feed = newStdinFeed(stdin, pipe)
	}
	var cancelObservedOnce sync.Once
	cmd.Cancel = func() error {
		if hooks.cancelObserved != nil {
			cancelObservedOnce.Do(hooks.cancelObserved)
		}
		if feed != nil {
			feed.stop()
		}
		return lifecycle.cancel()
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// Bound pipe draining when descendants retain inherited output handles.
	cmd.WaitDelay = 3 * time.Second

	start := hooks.start
	if start == nil {
		start = func(command *exec.Cmd) error { return command.Start() }
	}
	if err := start(cmd); err != nil {
		lifecycle.failStart()
		if feed != nil {
			feed.stop()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			contextErr := commandContextError(bin, args, ctxErr)
			return markRunStatus(errors.Join(contextErr, err), lifecycle.status())
		}
		return markRunStatus(err, lifecycle.status())
	}
	if feed != nil {
		feed.start()
	}
	if hooks.beforePublish != nil {
		hooks.beforePublish()
	}
	// On Windows, newProcessTree retains a process handle after Start; the
	// short attachment-window limitation is documented by that platform
	// implementation.
	var tree processTree
	if hooks.terminate != nil {
		tree = processTreeFunc(hooks.terminate)
	} else {
		var treeErr error
		tree, treeErr = newProcessTree(cmd)
		if treeErr != nil {
			// A platform tree is an enhancement. The direct process handle
			// remains a safe cancellation path when attachment is unavailable.
			tree = directProcessTree{}
		}
	}
	lifecycle.publishStart(tree)

	// Run owns the sole Wait call through the same lifecycle used by Stream.
	// This keeps context cancellation and the eventual reap from racing a
	// stale process-handle or numeric process-group signal.
	err := lifecycle.result()
	if feed != nil {
		// The child is gone. Stop the pump and close a closable reader so
		// a read blocked past the child's exit does not outlive the run
		// when the reader honors Close.
		feed.stop()
	}
	status := lifecycle.status()
	stdout, stderr := captured()
	commandErr := commandError(ctx, bin, identity, args, stdout, stderr, err)
	if commandErr == nil && ctx.Err() != nil && status.TerminatedByCancellation {
		commandErr = commandContextError(bin, args, ctx.Err())
	}
	if feed != nil {
		// commandError rebuilds a CLIError from an exit status and would
		// otherwise drop a read failure that happened while the command
		// also exited non-zero.
		if feedErr := feed.readError(); feedErr != nil && !errors.Is(commandErr, feedErr) {
			commandErr = errors.Join(commandErr, feedErr)
		}
	}
	return markRunStatus(commandErr, status)
}

// stdinStopGrace bounds how long command completion or cancellation waits for
// a stdin copy to observe a closed reader. Standard library readers unblock
// immediately; the bound only protects against an exotic Closer whose Close
// does not interrupt a concurrent Read.
const stdinStopGrace = time.Second

// stdinFeed copies a caller reader into a child's stdin pipe. The feed owns
// the pipe instead of handing the reader to os/exec, because Wait can block
// on a Stdin read that never returns even when WaitDelay is set. Owning the
// pipe keeps child termination independent of the caller's reader, and stop
// closes a closable reader so a blocked copy goroutine can exit when the
// command finishes or is cancelled. A non-closable reader cannot be
// interrupted; its copy goroutine may outlive the run until Read returns.
type stdinFeed struct {
	reader io.Reader
	pipe   io.WriteCloser
	done   chan struct{}

	mu      sync.Mutex
	started bool
	closed  bool
	readErr error
}

func newStdinFeed(reader io.Reader, pipe io.WriteCloser) *stdinFeed {
	return &stdinFeed{reader: reader, pipe: pipe, done: make(chan struct{})}
}

// start begins the copy after the child has started. If cancellation already
// closed the feed, no goroutine is created.
func (f *stdinFeed) start() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.started = true
	f.mu.Unlock()
	go f.copy()
}

func (f *stdinFeed) copy() {
	defer close(f.done)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := f.reader.Read(buffer)
		if n > 0 {
			if _, writeErr := f.pipe.Write(buffer[:n]); writeErr != nil {
				// The command stopped reading or the feed was stopped; a
				// write failure is expected teardown, not a caller error.
				_ = f.pipe.Close()
				return
			}
		}
		if readErr == nil {
			continue
		}
		_ = f.pipe.Close()
		if errors.Is(readErr, io.EOF) {
			return
		}
		f.mu.Lock()
		if !f.closed && f.readErr == nil {
			f.readErr = &StdinReadError{Err: readErr}
		}
		f.mu.Unlock()
		return
	}
}

// readError reports a reader failure observed while the command still owned
// stdin. A read unblocked by stop is teardown, not a caller error.
func (f *stdinFeed) readError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readErr
}

// stop ends the feed after the child is gone or cancellation terminates it.
// Closing the child's pipe releases a blocked write, and closing a closable
// reader releases a blocked read. The bounded wait keeps the copy goroutine
// from outliving the run when the reader honors Close; a non-closable reader
// or a Close that does not interrupt Read can leave the copy pending.
func (f *stdinFeed) stop() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	started := f.started
	f.mu.Unlock()
	_ = f.pipe.Close()
	closer, closable := f.reader.(io.Closer)
	if closable {
		_ = closer.Close()
	}
	if !started || !closable {
		return
	}
	timer := time.NewTimer(stdinStopGrace)
	defer timer.Stop()
	select {
	case <-f.done:
	case <-timer.C:
	}
}
