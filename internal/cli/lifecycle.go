package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
)

// processTree is the platform-specific, owned termination boundary for a
// command. A tree is published only after cmd.Start has returned successfully
// and is closed only after the sole cmd.Wait call has returned.
type processTree interface {
	terminate(*exec.Cmd) terminationResult
	close()
}

type terminationResult struct {
	// active is true only when the implementation proved that the direct
	// child was still active when termination was attempted. A successful
	// operation on an already-empty/zombie job is not active evidence.
	active bool
	err    error
}

// RunStatus describes what the local command lifecycle actually observed.
// Reaped is the fact that cmd.Wait returned; it is deliberately separate from
// whether cancellation won the race with that wait. TerminatedByCancellation
// is set only after a platform implementation proved the direct child was
// active and successfully accepted a termination action.
type RunStatus struct {
	Started                  bool
	Reaped                   bool
	CancelRequested          bool
	TerminatedByCancellation bool
}

type runStatusError struct {
	err    error
	status RunStatus
}

func (e *runStatusError) Error() string { return e.err.Error() }
func (e *runStatusError) Unwrap() error { return e.err }
func (e *runStatusError) RunStatus() RunStatus {
	return e.status
}

func markRunStatus(err error, status RunStatus) error {
	if err == nil {
		return nil
	}
	return &runStatusError{err: err, status: status}
}

// RunStatusOf returns the lifecycle status attached to err, when the runner
// supplied one. Test doubles and legacy wrappers may omit the capability.
func RunStatusOf(err error) (RunStatus, bool) {
	var status interface{ RunStatus() RunStatus }
	if !errors.As(err, &status) {
		return RunStatus{}, false
	}
	return status.RunStatus(), true
}

// StartedStatus is retained for callers of the earlier start-only status
// capability.
func StartedStatus(err error) (started, reported bool) {
	status, ok := RunStatusOf(err)
	return status.Started, ok
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

	cancelRequested          bool
	terminatedByCancellation bool
	terminateOnce            sync.Once
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

func (l *commandLifecycle) status() RunStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return RunStatus{
		Started:                  l.started,
		Reaped:                   l.reaped,
		CancelRequested:          l.cancelRequested,
		TerminatedByCancellation: l.terminatedByCancellation,
	}
}

func (l *commandLifecycle) isReaped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reaped
}

// cancel is the os/exec context callback. The optional argument is used by
// the stream Close path so an intentional close is not recorded as a
// cancellation race.
func (l *commandLifecycle) cancel() error {
	return l.terminate(true)
}

func (l *commandLifecycle) terminate(cancelled ...bool) error {
	contextTermination := len(cancelled) > 0 && cancelled[0]
	l.terminateOnce.Do(func() {
		// os/exec starts its context watcher during Start. Waiting here
		// makes cancellation before publication harmless and deterministic.
		<-l.startDone

		// Keep the lifecycle lock through the termination decision and
		// platform call. Wait cannot mark the child reaped between the active
		// check and the signal, so numeric handles and job state cannot race
		// the sole Wait owner.
		l.mu.Lock()
		defer l.mu.Unlock()
		if contextTermination {
			l.cancelRequested = true
		}
		if !l.started || l.startFailed || l.reaped {
			l.terminateErr = os.ErrProcessDone
			return
		}

		var result terminationResult
		if l.tree == nil {
			result = terminateDirectProcess(l.cmd)
		} else {
			result = l.tree.terminate(l.cmd)
		}
		l.terminateErr = result.err
		if contextTermination && result.err == nil && result.active {
			l.terminatedByCancellation = true
		}
	})
	l.mu.Lock()
	err := l.terminateErr
	l.mu.Unlock()
	return err
}

type directProcessTree struct{}

func (directProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcess(cmd)
}

func (directProcessTree) close() {}

type processTreeFunc func(*exec.Cmd) error

func (f processTreeFunc) terminate(cmd *exec.Cmd) terminationResult {
	return terminationResult{active: true, err: f(cmd)}
}
func (processTreeFunc) close() {}
