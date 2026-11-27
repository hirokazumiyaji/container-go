package wait

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
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

// ExecOutputTarget is an optional extension to Target for targets that can
// return a command's combined output. ForExec requires it only when an output
// matcher is configured, so targets that discard output keep working without
// changes. Implementations may bound the retained output; a matcher sees only
// the bytes the target reports, so a marker beyond that bound can never be
// observed. The container adapter retains at most 64 KiB.
type ExecOutputTarget interface {
	ExecCommandOutput(ctx context.Context, cmd []string) (code int, output []byte, err error)
}

// ExecOptions describes the execution context ForExec passes to a target
// implementing ExecOptionsTarget. Empty fields leave the corresponding
// container default unchanged.
type ExecOptions struct {
	// User is the container user that runs the readiness command.
	User string
	// WorkDir is the absolute working directory for the readiness command.
	WorkDir string
	// Env is added to the readiness command's environment.
	Env map[string]string
}

// ExecOptionsTarget is an optional extension to Target for targets that can
// run a readiness command with a user, working directory, and environment.
// ForExec requires it only when WithUser, WithWorkDir, or WithEnv is
// configured; a target without it fails immediately with a *ConfigError.
type ExecOptionsTarget interface {
	ExecCommandWithOptions(ctx context.Context, cmd []string, options ExecOptions) (code int, err error)
	ExecCommandOutputWithOptions(ctx context.Context, cmd []string, options ExecOptions) (code int, output []byte, err error)
}

// ExecStrategy waits until a command run inside the container exits
// with an accepted code (0 by default) and, when configured, produces
// acceptable output.
type ExecStrategy struct {
	options
	cmd               []string
	exitMatcher       func(int) bool
	outputMatcher     func([]byte) bool
	outputContains    string
	outputContainsSet bool
	execTimeout       time.Duration
	user              string
	workDir           string
	env               map[string]string
}

// ForExec waits for cmd to succeed inside the container.
func ForExec(cmd []string) *ExecStrategy {
	return &ExecStrategy{
		cmd:     slices.Clone(cmd),
		options: options{pollInterval: defaultExecPollInterval},
	}
}

// clone returns a strategy with an independent command slice. Builders derive
// a new value from the receiver so a strategy shared by parallel callers cannot
// be reconfigured through another caller's options.
func (s *ExecStrategy) clone() *ExecStrategy {
	cloned := *s
	cloned.cmd = slices.Clone(s.cmd)
	cloned.env = maps.Clone(s.env)
	return &cloned
}

// WithUser sets the container user that runs the readiness command. It needs
// a target implementing ExecOptionsTarget; the container adapter does.
func (s *ExecStrategy) WithUser(user string) *ExecStrategy {
	cloned := s.clone()
	cloned.user = user
	return cloned
}

// WithWorkDir sets the absolute working directory for the readiness command.
// It needs a target implementing ExecOptionsTarget; the container adapter
// does.
func (s *ExecStrategy) WithWorkDir(dir string) *ExecStrategy {
	cloned := s.clone()
	cloned.workDir = dir
	return cloned
}

// WithEnv adds environment variables to the readiness command. Values are
// registered as diagnostic secrets so a failure cannot echo them. It needs a
// target implementing ExecOptionsTarget; the container adapter does.
func (s *ExecStrategy) WithEnv(env map[string]string) *ExecStrategy {
	cloned := s.clone()
	if len(env) > 0 && cloned.env == nil {
		cloned.env = make(map[string]string, len(env))
	}
	for key, value := range env {
		cloned.env[key] = value
	}
	return cloned
}

func (s *ExecStrategy) execOptions() ExecOptions {
	options := ExecOptions{User: s.user, WorkDir: s.workDir}
	if len(s.env) > 0 {
		options.Env = maps.Clone(s.env)
	}
	return options
}

func (s *ExecStrategy) hasExecOptions() bool {
	return s.user != "" || s.workDir != "" || len(s.env) > 0
}

// WithExitCodeMatcher replaces the default exit-code-zero check.
func (s *ExecStrategy) WithExitCodeMatcher(matcher func(code int) bool) *ExecStrategy {
	cloned := s.clone()
	cloned.exitMatcher = matcher
	return cloned
}

// WithResponseMatcher accepts a poll only when matcher reports true for the
// command's output. The matcher runs only after the exit-code matcher accepts,
// so a rejected exit code stays the reported cause. ForExec needs an
// ExecOutputTarget for this; a target without it fails immediately with a
// *ConfigError. Without an output matcher, output is discarded as before.
func (s *ExecStrategy) WithResponseMatcher(matcher func(output []byte) bool) *ExecStrategy {
	cloned := s.clone()
	cloned.outputMatcher = matcher
	cloned.outputContains, cloned.outputContainsSet = "", false
	return cloned
}

// WithOutputContains accepts a poll only when the command's output contains
// substr. It is shorthand for WithResponseMatcher over bytes.Contains. An
// empty substr is rejected as a *ConfigError because it would accept any
// output.
func (s *ExecStrategy) WithOutputContains(substr string) *ExecStrategy {
	cloned := s.clone()
	cloned.outputContains, cloned.outputContainsSet = substr, true
	cloned.outputMatcher = func(output []byte) bool { return bytes.Contains(output, []byte(substr)) }
	return cloned
}

func (s *ExecStrategy) WithStartupTimeout(d time.Duration) *ExecStrategy {
	cloned := s.clone()
	cloned.startupTimeout = d
	return cloned
}

func (s *ExecStrategy) WithPollInterval(d time.Duration) *ExecStrategy {
	cloned := s.clone()
	cloned.pollInterval = d
	return cloned
}

// WithExecTimeout bounds each ExecCommand attempt. A hung command is
// canceled when the attempt expires and the poll retries until the startup
// timeout. Zero (the default) derives the bound from the poll interval as
// min(max(pollInterval*10, 1s), 5s); negative values are rejected before
// probing. The startup timeout still bounds the whole wait, so an explicit
// attempt timeout never extends the caller's budget.
func (s *ExecStrategy) WithExecTimeout(d time.Duration) *ExecStrategy {
	cloned := s.clone()
	cloned.execTimeout = d
	return cloned
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
	values = append(values, strings.Join(s.cmd, " "))
	for _, value := range s.env {
		values = append(values, value)
	}
	return values
}

func (s *ExecStrategy) validate() error {
	if err := s.options.validate("ForExec"); err != nil {
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
	if s.outputContainsSet && s.outputContains == "" {
		return &ConfigError{
			Strategy: "ForExec",
			Field:    "output substring",
			Reason:   "output substring must not be empty",
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
	var outputTarget ExecOutputTarget
	if s.outputMatcher != nil {
		var ok bool
		outputTarget, ok = target.(ExecOutputTarget)
		if !ok {
			return &ConfigError{
				Strategy: "ForExec",
				Field:    "target",
				Value:    fmt.Sprintf("%T", target),
				Reason:   "target does not report exec output; cannot apply an output matcher",
			}
		}
	}
	var optionsTarget ExecOptionsTarget
	if s.hasExecOptions() {
		var ok bool
		optionsTarget, ok = target.(ExecOptionsTarget)
		if !ok {
			return &ConfigError{
				Strategy: "ForExec",
				Field:    "target",
				Value:    fmt.Sprintf("%T", target),
				Reason:   "target does not support exec user, working directory, or environment options",
			}
		}
	}
	pollOptions := s.options
	if pollOptions.pollInterval == 0 {
		pollOptions.pollInterval = defaultExecPollInterval
	}
	attemptTimeout := s.attemptTimeout()
	return poll(ctx, pollOptions, target, fmt.Sprintf("wait for exec %v", s.cmd), func(ctx context.Context) error {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		var code int
		var output []byte
		var err error
		switch {
		case optionsTarget != nil && outputTarget != nil:
			code, output, err = optionsTarget.ExecCommandOutputWithOptions(attemptCtx, s.cmd, s.execOptions())
		case optionsTarget != nil:
			code, err = optionsTarget.ExecCommandWithOptions(attemptCtx, s.cmd, s.execOptions())
		case outputTarget != nil:
			code, output, err = outputTarget.ExecCommandOutput(attemptCtx, s.cmd)
		default:
			code, err = target.ExecCommand(attemptCtx, s.cmd)
		}
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
		if s.outputMatcher != nil && !s.outputMatcher(output) {
			return fmt.Errorf("exit code %d accepted but command output rejected", code)
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
