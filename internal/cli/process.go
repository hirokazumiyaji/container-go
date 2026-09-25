package cli

import (
	"os/exec"
	"sync"
)

// terminationResult records whether termination was actually signaled to
// the direct child. Exit status alone is not enough to distinguish a
// genuine backend failure from a platform kill during intentional
// cancellation.
type terminationResult struct {
	active bool
	err    error
}

// processTree is the platform-owned termination boundary for one command.
// Its handle or process-group identity remains valid until close, which is
// serialized with the sole command waiter by the owning stream lifecycle.
type processTree interface {
	terminate(*exec.Cmd) terminationResult
	close()
}

type processTreeFunc func(*exec.Cmd) error

func (f processTreeFunc) terminate(cmd *exec.Cmd) terminationResult {
	err := f(cmd)
	return terminationResult{active: err == nil, err: err}
}

func (processTreeFunc) close() {}

type directProcessTree struct{}

func (directProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcessResult(cmd)
}

func (directProcessTree) close() {}

type lazyProcessTree struct {
	mu   sync.Mutex
	tree processTree
}

func (l *lazyProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tree == nil {
		tree, err := newProcessTree(cmd)
		if err != nil {
			tree = directProcessTree{}
		}
		l.tree = tree
	}
	return l.tree.terminate(cmd)
}

func (l *lazyProcessTree) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tree != nil {
		l.tree.close()
	}
}
