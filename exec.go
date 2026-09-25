package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ExecOption configures Exec.
type ExecOption func(*execConfig) error

type execConfig struct {
	env     map[string]string
	user    string
	workdir string
}

// WithExecEnv sets environment variables for the exec'd process, passed via a
// temporary env file. It uses the same key and value validation as WithEnv.
// Exec returns ErrEnvFileUnsupported on Windows.
func WithExecEnv(env map[string]string) ExecOption {
	return func(c *execConfig) error {
		if key, _, err := firstInvalidEnv(env); err != nil {
			return fmt.Errorf("invalid exec environment variable %q: %w", key, err)
		}
		for k, v := range env {
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

// Exec runs a command in the container and returns its exit code and
// combined output. A non-zero exit code is a result, not an error.
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (exitCode int, output io.Reader, retErr error) {
	if len(cmd) == 0 {
		return 0, nil, errors.New("exec: command must not be empty")
	}
	cfg := &execConfig{env: map[string]string{}}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return 0, nil, err
		}
	}

	var envFile, envDir string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFileContext(ctx, cfg.env)
		if err != nil {
			if dir != "" {
				// Preserve ownership when a late root-lock error is
				// returned with a published env directory.
				defer func() {
					if retryErr := retryEnvFileCleanupWithError(&dir); retryErr != nil {
						retErr = joinEnvFileCleanupError(retErr, retryErr)
					}
				}()
				return 0, nil, joinEnvFileCleanupError(err, cleanupEnvFileWithRetry(dir))
			}
			return 0, nil, err
		}
		envFile, envDir = path, dir
		defer func() {
			if retryErr := retryEnvFileCleanupWithError(&envDir); retryErr != nil {
				retErr = joinEnvFileCleanupError(retErr, retryErr)
			}
		}()
	}

	stdout, stderr, err := c.runner.Run(ctx, c.eng.execArgs(c.id, cfg, envFile, cmd)...)
	// The CLI has finished reading the env file. Remove it before any
	// result classification or caller-visible output processing. Retain
	// envDir for a deferred retry if removal fails, and preserve the error.
	envCleanupErr := cleanupEnvFileAfterUseContext(ctx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}
	output = io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	if err == nil {
		return 0, output, envCleanupErr
	}
	if !cli.IsCommandExit(err) {
		return 0, nil, joinEnvFileCleanupError(wrapNotFound(c.classify(ctx, err)), envCleanupErr)
	}
	var cliErr *cli.CLIError
	errors.As(err, &cliErr)
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFound(err) && !maybeInfraExecErr(err) {
		return cliErr.ExitCode, output, envCleanupErr
	}
	if c.execContainerRunning(ctx) {
		return cliErr.ExitCode, output, envCleanupErr
	}
	return 0, nil, joinEnvFileCleanupError(wrapNotFound(c.classify(ctx, err)), envCleanupErr)
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
