package cli

import (
	"context"
	"os"
	"os/exec"
	"sync"
)

// processTree is the platform-specific, owned termination boundary for a
// command. A tree is published only after cmd.Start has returned successfully
// and is closed only after the sole cmd.Wait call has returned.
type processTree interface {
	terminate(*exec.Cmd) error
	close()
}

type commandLifecycle struct {
	cmd *exec.Cmd
	ctx context.Context

	startDone chan struct{}
	waitDone  chan struct{}
	waitOnce  sync.Once

	mu           sync.Mutex
	started      bool
	startFailed  bool
	reaped       bool
	tree         processTree
	waitErr      error
	ctxErr       error
	terminateErr error

	terminateOnce sync.Once
}

func newCommandLifecycle(ctx context.Context, cmd *exec.Cmd) *commandLifecycle {
	return &commandLifecycle{
		cmd:       cmd,
		ctx:       ctx,
		startDone: make(chan struct{}),
		waitDone:  make(chan struct{}),
	}
}

// publishStart transfers ownership of a successfully started command to the
// lifecycle owner. The context watcher may call Cancel as soon as Start
// returns, so publication and the start barrier are one operation.
func (l *commandLifecycle) publishStart(tree processTree) {
	l.mu.Lock()
	l.started = true
	l.tree = tree
	l.mu.Unlock()
	close(l.startDone)
}

func (l *commandLifecycle) failStart() {
	l.mu.Lock()
	l.startFailed = true
	l.mu.Unlock()
	close(l.startDone)
}

// wait is the only place that calls cmd.Wait. In particular, no other
// lifecycle path may reap the process and then race a numeric process-group
// signal against that reap.
func (l *commandLifecycle) wait() {
	l.waitOnce.Do(func() {
		err := l.cmd.Wait()

		l.mu.Lock()
		l.waitErr = err
		l.ctxErr = l.ctx.Err()
		l.reaped = true
		tree := l.tree
		l.mu.Unlock()

		// On platforms with a job object this also releases any descendants
		// that outlived the direct child. The tree implementations serialize
		// this close with a concurrent termination request.
		if tree != nil {
			tree.close()
		}
		close(l.waitDone)
	})
	<-l.waitDone
}

func (l *commandLifecycle) result() error {
	l.wait()
	return l.error()
}

func (l *commandLifecycle) error() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waitErr
}

func (l *commandLifecycle) contextError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ctxErr
}

func (l *commandLifecycle) terminate() error {
	l.terminateOnce.Do(func() {
		// os/exec starts its context watcher during Start. Waiting here
		// makes cancellation before publication harmless and deterministic.
		<-l.startDone

		l.mu.Lock()
		if !l.started || l.startFailed || l.reaped {
			l.mu.Unlock()
			l.terminateErr = os.ErrProcessDone
			return
		}
		tree := l.tree
		cmd := l.cmd
		l.mu.Unlock()

		if tree == nil {
			// The direct handle is synchronized with the sole Wait call and
			// cannot be redirected to a reused PID.
			l.terminateErr = cmd.Process.Kill()
			return
		}
		// Platform trees use the direct process handle (or a Job Object
		// handle) as their identity gate before any group-wide operation.
		l.terminateErr = tree.terminate(cmd)
	})
	return l.terminateErr
}

func (l *commandLifecycle) isReaped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reaped
}

type directProcessTree struct{}

func (directProcessTree) terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}

func (directProcessTree) close() {}

type processTreeFunc func(*exec.Cmd) error

func (f processTreeFunc) terminate(cmd *exec.Cmd) error { return f(cmd) }
func (processTreeFunc) close()                          {}
