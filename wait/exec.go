package wait

import (
	"context"
	"fmt"
	"time"
)

// ExecStrategy waits until a command run inside the container exits
// with an accepted code (0 by default).
type ExecStrategy struct {
	options
	cmd         []string
	exitMatcher func(int) bool
}

// ForExec waits for cmd to succeed inside the container.
func ForExec(cmd []string) *ExecStrategy {
	return &ExecStrategy{cmd: cmd}
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
	matcher := s.exitMatcher
	if matcher == nil {
		matcher = func(code int) bool { return code == 0 }
	}
	return poll(ctx, s.options, target, fmt.Sprintf("wait for exec %v", s.cmd), func(ctx context.Context) error {
		code, err := target.ExecCommand(ctx, s.cmd)
		if err != nil {
			return err
		}
		if !matcher(code) {
			return fmt.Errorf("exit code %d not accepted", code)
		}
		return nil
	})
}
