package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ExecOption configures Exec.
type ExecOption func(*execConfig) error

// defaultExecTimeout is a var so tests can shorten the public Exec
// deadline without waiting for the production default.
var defaultExecTimeout = 30 * time.Second

type execConfig struct {
	env        map[string]string
	user       string
	workdir    string
	timeout    time.Duration
	timeoutSet bool
}

// WithExecTimeout sets an upper bound for one Exec invocation. A
// positive d and the caller's deadline are combined, with the earlier
// deadline winning. The default is 30 seconds. A zero value disables
// only the library default for deliberately long-running commands;
// callers should then pass a cancellable context.
func WithExecTimeout(d time.Duration) ExecOption {
	return func(c *execConfig) error {
		if d < 0 {
			return fmt.Errorf("exec timeout must not be negative, got %v", d)
		}
		c.timeout = d
		c.timeoutSet = true
		return nil
	}
}

// WithExecEnv sets environment variables for the exec'd process,
// passed via a temporary env file.
func WithExecEnv(env map[string]string) ExecOption {
	return func(c *execConfig) error {
		for k, v := range env {
			if k == "" || strings.ContainsAny(k, "=\n\x00") || strings.ContainsAny(v, "\n\x00") {
				return fmt.Errorf("invalid exec environment variable %q", k)
			}
			c.env[k] = v
		}
		return nil
	}
}

// WithExecUser sets the user for the exec'd process.
func WithExecUser(u string) ExecOption {
	return func(c *execConfig) error {
		if !userRE.MatchString(u) {
			return fmt.Errorf("invalid exec user %q", u)
		}
		c.user = u
		return nil
	}
}

// WithExecWorkDir sets the working directory for the exec'd process.
func WithExecWorkDir(dir string) ExecOption {
	return func(c *execConfig) error {
		if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\n\x00") {
			return fmt.Errorf("exec working directory %q must be an absolute path", dir)
		}
		c.workdir = dir
		return nil
	}
}

func withExecTimeout(ctx context.Context, cfg *execConfig) (context.Context, context.CancelFunc) {
	if cfg.timeoutSet {
		if cfg.timeout == 0 {
			return ctx, func() {}
		}
		// WithTimeout preserves the earlier deadline when ctx already
		// has one, while still bounding a caller that supplied a later
		// deadline.
		return context.WithTimeout(ctx, cfg.timeout)
	}
	return withDefaultTimeout(ctx, cfg.timeout)
}

// Exec runs a command in the container and returns its exit code and
// combined output. A non-zero exit code is a result, not an error. When
// the backend or the context fails, the reader still contains whatever
// stdout and stderr the command produced before the failure; callers
// should read it even when err is non-nil. If the CLI reports an exit
// status, that status is returned alongside the classified error.
// When a context error terminates the local CLI, Exec returns an
// ExecTerminationError even if the operating system represents the killed
// process with a normal exit status. The backend-side process may still be
// running because neither supported CLI exposes a common exec-instance kill.
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error) {
	if len(cmd) == 0 {
		return 0, nil, errors.New("exec: command must not be empty")
	}
	cfg := &execConfig{env: map[string]string{}, timeout: defaultExecTimeout}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return 0, nil, err
		}
	}

	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return 0, nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}

	// Exec is a buffered, bounded operation by default. An explicit zero
	// timeout opts out of the library default. A positive custom timeout
	// is combined with the caller's deadline, so the earlier one wins.
	execCtx, cancel := withExecTimeout(ctx, cfg)
	defer cancel()

	stdout, stderr, err := c.runner.Run(execCtx, c.eng.execArgs(c.id, cfg, envFile, cmd)...)
	output := io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	if err == nil {
		return 0, output, nil
	}
	if !cli.IsCommandExit(err) {
		// A launch, context, or transport failure has no command exit
		// code, but the CLI may still have emitted useful diagnostics.
		classified := wrapNotFound(c.classify(execCtx, err))
		if isExecContextError(err) {
			classified = &ExecTerminationError{Err: classified}
		}
		return 0, output, classified
	}
	var cliErr *cli.CLIError
	errors.As(err, &cliErr)
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFound(err) && !maybeInfraExecErr(err) {
		if contextErr := execContextResultError(err, cliErr); contextErr != nil {
			return cliErr.ExitCode, output, contextErr
		}
		return cliErr.ExitCode, output, nil
	}
	if c.execContainerRunning(execCtx) {
		if contextErr := execContextResultError(err, cliErr); contextErr != nil {
			return cliErr.ExitCode, output, contextErr
		}
		return cliErr.ExitCode, output, nil
	}
	// Preserve both the CLI exit code and the classified infrastructure
	// error. The output reader is intentionally non-nil on this path.
	classified := wrapNotFound(c.classify(execCtx, err))
	if isExecContextError(err) {
		classified = &ExecTerminationError{Err: classified}
	}
	return cliErr.ExitCode, output, classified
}

func isExecContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func execContextResultError(err error, _ *cli.CLIError) error {
	if !isExecContextError(err) {
		return nil
	}
	return &ExecTerminationError{Err: err}
}

// maybeInfraExecErr reports whether an exec CLIError could be about the
// execution substrate rather than the app process. Generic app output
// returns false so normal non-zero exits cost no extra probe.
func maybeInfraExecErr(err error) bool {
	s, ok := execCLIStderr(err)
	if !ok {
		return true
	}
	for _, sub := range []string{
		"daemon", "cannot connect", "connection refused", "xpc",
		"backend", "socket", "is not running", "not running",
		"stopped", "paused", "restarting", "removing", "no such",
	} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func execCLIStderr(err error) (string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return "", false
	}
	return strings.ToLower(cliErr.Stderr), true
}

// execContainerRunning verifies via inspect that the container is still
// running. App-level failures keep their exit code; missing, stopped,
// or unreachable containers report an error.
func (c *Container) execContainerRunning(ctx context.Context) bool {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(c.id)...)
	if err != nil {
		return false
	}
	info, err := c.eng.parseInspect(stdout, c.id)
	if err != nil {
		return false
	}
	return info.state == StateRunning
}
