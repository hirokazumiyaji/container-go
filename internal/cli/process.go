package cli

import "os/exec"

// terminationResult records whether termination was actually signaled to
// the direct child. syntheticExit is reserved for platforms whose kill API
// reports a synthetic positive exit status, such as Windows
// TerminateProcess; it must not suppress a genuine positive Unix exit.
type terminationResult struct {
	active        bool
	syntheticExit bool
	err           error
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
