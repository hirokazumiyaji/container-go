// Package cli executes a container backend CLI (`container` or `docker`)
// as a child process and classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

// Runner executes one backend CLI invocation.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
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
	err := r.execute(ctx, hooks, &stdout, &stderr, func() ([]byte, []byte) {
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
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stdoutCount := &countingWriter{dst: stdout}
	stderrCount := &countingWriter{dst: stderr}
	diagnostic := newTailBuffer(maxStderrContext)
	runErr := r.execute(ctx, runHooks{}, stdoutCount, &diagnosticWriter{output: stderrCount, diagnostic: diagnostic}, func() ([]byte, []byte) {
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

// execute owns one command lifecycle. stdoutW and stderrW receive the
// child's streams; captured returns the stdout and stderr bytes available
// for the CLIError diagnostic once the child has been reaped.
func (r *ExecRunner) execute(ctx context.Context, hooks runHooks, stdoutW, stderrW io.Writer, captured func() ([]byte, []byte), args []string) error {
	bin := r.binary()
	identity := r.identity()
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(r.Env) > 0 {
		cmd.Env = MergeEnv(os.Environ(), r.Env)
	}
	configureProcessTree(cmd)
	lifecycle := newCommandLifecycle(ctx, cmd)
	var cancelObservedOnce sync.Once
	cmd.Cancel = func() error {
		if hooks.cancelObserved != nil {
			cancelObservedOnce.Do(hooks.cancelObserved)
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			contextErr := commandContextError(bin, args, ctxErr)
			return markRunStatus(errors.Join(contextErr, err), lifecycle.status())
		}
		return markRunStatus(err, lifecycle.status())
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
	status := lifecycle.status()
	stdout, stderr := captured()
	commandErr := commandError(ctx, bin, identity, args, stdout, stderr, err)
	if commandErr == nil && ctx.Err() != nil && status.TerminatedByCancellation {
		commandErr = commandContextError(bin, args, ctx.Err())
	}
	return markRunStatus(commandErr, status)
}

// tailBuffer is a mutex-protected diagnostic ring buffer. It always reports
// complete writes and retains the last limit bytes without shifting the whole
// window on every write.
type tailBuffer struct {
	mu      sync.Mutex
	tail    *diagnostic.TailBuffer
	limit   int
	written int64
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{tail: diagnostic.NewTailBuffer(limit), limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written += int64(len(p))
	return b.tail.Write(p)
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := b.tail.String()
	if b.written > int64(b.limit) {
		// The window starts mid-stream. Drop the incomplete first line so a
		// structural secret whose prefix was discarded cannot be emitted.
		data = trimPartialLeadingLine(data)
	}
	return data
}

// trimPartialLeadingLine removes an incomplete first line from a truncated
// diagnostic window. A window with no line break has no safe prefix left.
// Progress output overwrites lines with carriage returns, so both separators
// end a line.
func trimPartialLeadingLine(s string) string {
	i := strings.IndexAny(s, "\r\n")
	if i < 0 {
		return ""
	}
	i++
	if s[i-1] == '\r' && i < len(s) && s[i] == '\n' {
		i++
	}
	return s[i:]
}
