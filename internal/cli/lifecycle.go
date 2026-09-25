package cli

import (
	"context"
	"os"
	"os/exec"
	"sync"
)

type processTree interface {
	terminate(*exec.Cmd) error
	close()
}

type directProcessTree struct{}

func (directProcessTree) terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}

func (directProcessTree) close() {}

// commandLifecycle makes the started command, cancellation, and the sole
// Wait call one ownership boundary. In particular, a context watcher never
// races a post-Wait numeric process-group signal.
type commandLifecycle struct {
	cmd *exec.Cmd
	ctx context.Context

	startDone chan struct{}
	waitDone  chan struct{}
	waitOnce  sync.Once

	mu            sync.Mutex
	started       bool
	startFailed   bool
	reaped        bool
	tree          processTree
	waitErr       error
	terminateErr  error
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

func (l *commandLifecycle) wait() {
	l.waitOnce.Do(func() {
		err := l.cmd.Wait()
		l.mu.Lock()
		l.waitErr = err
		l.reaped = true
		tree := l.tree
		l.mu.Unlock()
		if tree != nil {
			tree.close()
		}
		close(l.waitDone)
	})
	<-l.waitDone
}

func (l *commandLifecycle) result() error {
	l.wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waitErr
}

func (l *commandLifecycle) terminate() error {
	l.terminateOnce.Do(func() {
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
			l.terminateErr = directProcessTree{}.terminate(cmd)
			return
		}
		l.terminateErr = tree.terminate(cmd)
	})
	return l.terminateErr
}
