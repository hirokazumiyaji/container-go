package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ExecOption configures Exec.
type ExecOption func(*execConfig) error

type execConfig struct {
	env      map[string]string
	user     string
	workdir  string
	maxBytes int64
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

// WithExecMaxBytes caps the combined stdout+stderr retained by Exec or
// ExecTo. The limit must be positive; leaving the option unset keeps
// the historical full-output behavior. Streaming paths merge the two
// streams in arrival order.
func WithExecMaxBytes(maxBytes int64) ExecOption {
	return func(c *execConfig) error {
		if maxBytes <= 0 {
			return fmt.Errorf("exec MaxBytes must be greater than zero, got %d", maxBytes)
		}
		c.maxBytes = maxBytes
		return nil
	}
}

// Exec runs a command in the container and returns its exit code and
// combined output. A non-zero exit code is a result, not an error. The
// historical full-output behavior remains when WithExecMaxBytes is not
// supplied; bounded callers should set that option.
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error) {
	cfg, envFile, cleanup, err := prepareExec(cmd, opts)
	if err != nil {
		return 0, nil, err
	}
	defer cleanup()

	args := c.eng.execArgs(c.id, cfg, envFile, cmd)
	var output io.Reader
	if cfg.maxBytes > 0 {
		capture := newBoundedBuffer(cfg.maxBytes)
		_, err = cli.RunTo(c.runner, ctx, capture, capture, args...)
		output = capture.reader()
	} else {
		var stdout, stderr []byte
		stdout, stderr, err = c.runner.Run(ctx, args...)
		output = io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	}

	code, resolvedErr := c.resolveExecError(ctx, err)
	if resolvedErr != nil {
		return code, nil, resolvedErr
	}
	return code, output, nil
}

// ExecTo runs a command and streams its combined stdout+stderr to
// output while the command is running. WithExecMaxBytes applies a hard
// limit to that stream; bytes beyond it are drained and discarded. The
// returned stats count all bytes observed, not just bytes forwarded.
func (c *Container) ExecTo(ctx context.Context, cmd []string, output io.Writer, opts ...ExecOption) (int, OutputStats, error) {
	cfg, envFile, cleanup, err := prepareExec(cmd, opts)
	if err != nil {
		return 0, OutputStats{}, err
	}
	defer cleanup()

	var sink io.Writer
	var limited *limitedWriter
	if cfg.maxBytes > 0 {
		limited = limitedStreamWriter(output, cfg.maxBytes)
		sink = limited
	} else {
		sink = streamWriter(output)
	}
	stats, runErr := cli.RunTo(c.runner, ctx, sink, sink, c.eng.execArgs(c.id, cfg, envFile, cmd)...)
	result := OutputStats{
		Bytes:     stats.StdoutBytes + stats.StderrBytes,
		Truncated: stats.Truncated(),
	}
	if limited != nil {
		result.Truncated = result.Truncated || limited.Truncated()
	}
	code, resolvedErr := c.resolveExecError(ctx, runErr)
	if resolvedErr != nil {
		return code, result, resolvedErr
	}
	return code, result, nil
}

func prepareExec(cmd []string, opts []ExecOption) (*execConfig, string, func(), error) {
	if len(cmd) == 0 {
		return nil, "", func() {}, errors.New("exec: command must not be empty")
	}
	cfg := &execConfig{env: map[string]string{}}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, "", func() {}, err
		}
	}

	if len(cfg.env) == 0 {
		return cfg, "", func() {}, nil
	}
	path, dir, err := writeEnvFile(cfg.env)
	if err != nil {
		return nil, "", func() {}, err
	}
	return cfg, path, func() { _ = os.RemoveAll(dir) }, nil
}

// resolveExecError preserves application exit results while retaining
// the existing infrastructure/not-found classification behavior.
func (c *Container) resolveExecError(ctx context.Context, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if !cli.IsCommandExit(err) {
		return 0, wrapNotFound(c.classify(ctx, err))
	}
	var cliErr *cli.CLIError
	_ = errors.As(err, &cliErr)
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFound(err) && !maybeInfraExecErr(err) {
		return cliErr.ExitCode, nil
	}
	if c.execContainerRunning(ctx) {
		return cliErr.ExitCode, nil
	}
	return 0, wrapNotFound(c.classify(ctx, err))
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
