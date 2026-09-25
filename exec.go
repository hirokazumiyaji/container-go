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
	env     map[string]string
	user    string
	workdir string
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

// Exec runs a command in the container and returns its exit code and
// combined output. A non-zero exit code is a result, not an error.
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error) {
	if len(cmd) == 0 {
		return 0, nil, errors.New("exec: command must not be empty")
	}
	cfg := &execConfig{env: map[string]string{}}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return 0, nil, err
		}
	}
	target, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return 0, nil, err
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

	stdout, stderr, err := c.runner.Run(ctx, c.eng.execArgs(target, cfg, envFile, cmd)...)
	output := io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	if err == nil {
		return 0, output, nil
	}
	if !cli.IsCommandExit(err) {
		return 0, nil, wrapNotFound(c.classify(ctx, err))
	}
	var cliErr *cli.CLIError
	errors.As(err, &cliErr)
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFound(err) && !maybeInfraExecErr(err) {
		return cliErr.ExitCode, output, nil
	}
	state, inspectErr := c.verifyExecContainer(ctx)
	if inspectErr != nil {
		return 0, nil, errors.Join(err, inspectErr)
	}
	if state == StateRunning {
		return cliErr.ExitCode, output, nil
	}
	return 0, nil, wrapNotFound(c.classify(ctx, err))
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

// verifyExecContainer verifies the current state while preserving the
// precise inspect error for callers that need to distinguish a missing
// container from malformed or otherwise unusable output.
func (c *Container) verifyExecContainer(ctx context.Context) (State, error) {
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return StateUnknown, err
	}
	return info.state, nil
}

// execContainerRunning is kept as the boolean compatibility helper for
// package-local callers; Exec uses verifyExecContainer so inspect errors
// are not reduced to false.
//
//nolint:unused
func (c *Container) execContainerRunning(ctx context.Context) bool {
	state, err := c.verifyExecContainer(ctx)
	return err == nil && state == StateRunning
}
