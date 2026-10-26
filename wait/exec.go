package wait

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// defaultExecPollInterval is coarser than the connection-based default
	// because each ForExec check already spawns a CLI process.
	defaultExecPollInterval = 250 * time.Millisecond

	// The derived per-attempt timeout grows with the poll interval so slow
	// probes get proportionally more room, but it stays within bounds that
	// leave the startup budget usable for retries.
	execAttemptTimeoutFactor = 10
	minExecAttemptTimeout    = time.Second
	maxExecAttemptTimeout    = 5 * time.Second
)

// ExecStrategy waits until a command run inside the container exits
// with an accepted code (0 by default).
type ExecStrategy struct {
	options
	cmd         []string
	exitMatcher func(int) bool
	execTimeout time.Duration
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

// WithExecTimeout bounds each ExecCommand attempt. A hung command is
// canceled when the attempt expires and the poll retries until the startup
// timeout. Zero (the default) derives the bound from the poll interval as
// min(max(pollInterval*10, 1s), 5s); negative values are rejected before
// probing. The startup timeout still bounds the whole wait, so an explicit
// attempt timeout never extends the caller's budget.
func (s *ExecStrategy) WithExecTimeout(d time.Duration) *ExecStrategy {
	s.execTimeout = d
	return s
}

// attemptTimeout reports the bound applied to one ExecCommand call.
func (s *ExecStrategy) attemptTimeout() time.Duration {
	if s.execTimeout > 0 {
		return s.execTimeout
	}
	interval := s.pollInterval
	if interval <= 0 {
		interval = defaultExecPollInterval
	}
	if interval >= maxExecAttemptTimeout/execAttemptTimeoutFactor {
		return maxExecAttemptTimeout
	}
	timeout := interval * execAttemptTimeoutFactor
	if timeout < minExecAttemptTimeout {
		timeout = minExecAttemptTimeout
	}
	return timeout
}

func (s *ExecStrategy) DiagnosticValues() []string {
	values := append([]string(nil), s.cmd...)
	return append(values, strings.Join(s.cmd, " "))
}

func (s *ExecStrategy) validate() error {
	if err := s.options.validate(); err != nil {
		return err
	}
	if s.execTimeout < 0 {
		return &ConfigError{
			Strategy: "ForExec",
			Field:    "exec timeout",
			Value:    s.execTimeout.String(),
			Reason:   "exec timeout must not be negative",
		}
	}
	if len(s.cmd) == 0 {
		return &ConfigError{
			Strategy: "ForExec",
			Field:    "command",
			Reason:   "command must not be empty",
		}
	}
	if s.cmd[0] == "" {
		return &ConfigError{
			Strategy: "ForExec",
			Field:    "executable",
			Reason:   "executable must not be empty",
		}
	}
	return nil
}

func (s *ExecStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	defer func() { retErr = safeDiagnosticError(retErr, s.DiagnosticValues()...) }()
	if err := s.validate(); err != nil {
		return err
	}
	matcher := s.exitMatcher
	if matcher == nil {
		matcher = func(code int) bool { return code == 0 }
	}
	pollOptions := s.options
	if pollOptions.pollInterval == 0 {
		pollOptions.pollInterval = defaultExecPollInterval
	}
	attemptTimeout := s.attemptTimeout()
	return poll(ctx, pollOptions, target, fmt.Sprintf("wait for exec %v", s.cmd), func(ctx context.Context) error {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		code, err := target.ExecCommand(attemptCtx, s.cmd)
		attemptErr := attemptCtx.Err()
		cancel()
		if attemptErr != nil {
			if ctx.Err() != nil {
				// The caller or startup budget ended the attempt. poll
				// reports that termination cause itself.
				return err
			}
			// A failed attempt must not consume the startup budget or be
			// mistaken for a terminal configuration error. Keep the command
			// error as text only: a killed process can surface as a
			// launch-looking error, but the deadline belongs to this
			// attempt, not to the strategy.
			return &execAttemptTimeoutError{
				timeout: attemptTimeout,
				cause:   err,
			}
		}
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
	})
}

// execAttemptTimeoutError reports one ExecCommand attempt that exceeded its
// own bound. The command error is retained for diagnostics but is not part of
// the unwrap chain, so a launch-shaped error produced while the process was
// being killed cannot be classified as permanent.
type execAttemptTimeoutError struct {
	timeout time.Duration
	cause   error
}

func (e *execAttemptTimeoutError) Error() string {
	message := fmt.Sprintf("exec attempt timed out after %v", e.timeout)
	// A context-only cause repeats the deadline this error already reports.
	if e.cause != nil && !errors.Is(e.cause, context.Canceled) && !errors.Is(e.cause, context.DeadlineExceeded) {
		message += ": " + e.cause.Error()
	}
	return message
}

func (e *execAttemptTimeoutError) Unwrap() error { return context.DeadlineExceeded }
