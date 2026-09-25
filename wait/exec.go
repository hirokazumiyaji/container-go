package wait

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// defaultExecPollInterval is coarser than the connection-based default
// because each ForExec check already spawns a CLI process.
const defaultExecPollInterval = 250 * time.Millisecond

// ExecStrategy waits until a command run inside the container exits
// with an accepted code (0 by default).
type ExecStrategy struct {
	options
	cmd         []string
	exitMatcher func(int) bool
}

// ForExec waits for cmd to succeed inside the container.
func ForExec(cmd []string) *ExecStrategy {
	return &ExecStrategy{
		cmd:     cmd,
		options: options{pollInterval: defaultExecPollInterval},
	}
}

// WithExitCodeMatcher replaces the default exit-code-zero check.
func (s *ExecStrategy) WithExitCodeMatcher(matcher func(code int) bool) *ExecStrategy {
	s.exitMatcher = matcher
	return s
}

func (s *ExecStrategy) WithStartupTimeout(d time.Duration) *ExecStrategy {
	s.startupTimeout = d
	return s
}

func (s *ExecStrategy) WithPollInterval(d time.Duration) *ExecStrategy {
	s.pollInterval = d
	return s
}

func (s *ExecStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if len(s.cmd) == 0 {
		return errors.New("wait for exec: command must not be empty")
	}
	matcher := s.exitMatcher
	if matcher == nil {
		matcher = func(code int) bool { return code == 0 }
	}
	// checkRunning is false: each check already talks to the container
	// via exec, so a concurrent Running probe would only add spawns.
	// A stopped container is still reported once at timeout.
	return poll(ctx, s.options, target, fmt.Sprintf("wait for exec %v", s.cmd), func(ctx context.Context) error {
		code, err := target.ExecCommand(ctx, s.cmd)
		if err != nil {
			// Command exits are returned as codes. Only a CLI launch
			// failure (*exec.Error) is known to be permanent; other
			// errors may be transient and are retried until timeout.
			var launchErr *exec.Error
			if errors.As(err, &launchErr) {
				return fatalCheckError{err: err}
			}
			return err
		}
		if !matcher(code) {
			return fmt.Errorf("exit code %d not accepted", code)
		}
		return nil
	}, false, s.cmd...)
}
