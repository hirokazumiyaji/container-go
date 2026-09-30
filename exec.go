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

	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return 0, nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}

	stdout, stderr, err := c.runner.Run(ctx, c.eng.execArgs(c.id, cfg, envFile, cmd)...)
	output := io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	// A canceled ExecRunner can join a command exit with the caller
	// context. Preserve both but prioritize cancellation as an API error;
	// otherwise the exit code could be mistaken for a workload result or
	// an object-absence diagnostic.
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		return 0, nil, contextErr
	}
	if err == nil {
		return 0, output, nil
	}
	if !cli.IsCommandExit(err) {
		return 0, nil, wrapNotFoundFor(c.eng, c.classify(ctx, err))
	}
	var cliErr *cli.CLIError
	errors.As(err, &cliErr)
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFoundFor(c.eng, err) && !maybeInfraExecErr(c.eng, err) {
		if contextErr := commandContextError(ctx, nil); contextErr != nil {
			return 0, nil, errors.Join(err, contextErr)
		}
		return cliErr.ExitCode, output, nil
	}
	inspection := c.inspectExecTarget(ctx)
	// Verification uses a derived timeout. Recheck the caller and returned
	// inspection chain so cancellation remains authoritative at this boundary.
	if contextErr := commandContextError(ctx, inspection.err); contextErr != nil {
		return 0, nil, errors.Join(err, contextErr)
	}
	switch inspection.state {
	case execTargetRunning:
		if contextErr := commandContextError(ctx, nil); contextErr != nil {
			return 0, nil, errors.Join(err, contextErr)
		}
		return cliErr.ExitCode, output, nil
	case execTargetNotFound:
		cause := error(err)
		if inspection.err != nil {
			cause = errors.Join(err, inspection.err)
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrContainerNotFound, cause)
	case execTargetStopped:
		return 0, nil, err
	default:
		inspectErr := inspection.err
		if maybeInfraInspectErr(c.eng, inspectErr) {
			inspectErr = c.classify(ctx, inspectErr)
		}
		return 0, nil, errors.Join(err, inspectErr)
	}
}

// maybeInfraExecErr reports whether an exec CLIError could be about the
// execution substrate rather than the app process. Generic app output
// returns false so normal non-zero exits cost no extra probe.
func maybeInfraExecErr(eng engine, err error) bool {
	ctx, ok := backendCLIError(err, eng.binary())
	if !ok || ctx.operation != "exec" {
		return false
	}
	lines, ok := cliErrorLines(err)
	if !ok {
		return true
	}
	for _, line := range lines {
		for _, sub := range []string{
			"daemon", "cannot connect", "connection refused", "xpc",
			"backend", "socket", "system is not running", "is not running",
			"stopped", "paused", "restarting", "removing",
		} {
			if strings.Contains(line, sub) {
				return true
			}
		}
	}
	return false
}

// maybeInfraInspectErr reports whether a failed verification inspect still
// carries backend-reachability evidence worth classifying. Object mismatch,
// application, parse, TLS, and endpoint-configuration errors are not probe
// failures and remain ordinary diagnostic errors.
func maybeInfraInspectErr(eng engine, err error) bool {
	if cli.IsProbeConfigurationError(err) {
		return false
	}
	ctx, ok := backendCLIError(err, eng.binary())
	if !ok || ctx.operation != "inspect" {
		return false
	}
	lines, ok := cliErrorLines(err)
	if !ok {
		return true
	}
	for _, line := range lines {
		for _, sub := range []string{
			"daemon", "cannot connect", "connection refused", "xpc",
			"backend", "socket", "system is not running", "is not running",
		} {
			if strings.Contains(line, sub) {
				return true
			}
		}
	}
	return false
}

type execTargetState uint8

const (
	execTargetInspectionFailed execTargetState = iota
	execTargetRunning
	execTargetStopped
	execTargetNotFound
)

type execTargetInspection struct {
	state execTargetState
	err   error
}

// inspectExecTarget distinguishes a reachable running/stopped target from
// a target-matched absence and from an inspect failure. A TLS, parsing, or
// application error is never collapsed into "not found".
func (c *Container) inspectExecTarget(ctx context.Context) execTargetInspection {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(c.id)...)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return execTargetInspection{state: execTargetInspectionFailed, err: contextErr}
	}
	if err != nil {
		if !cli.IsProbeConfigurationError(err) && isNotFoundFor(c.eng, err) {
			return execTargetInspection{state: execTargetNotFound, err: err}
		}
		return execTargetInspection{state: execTargetInspectionFailed, err: err}
	}
	info, err := c.eng.parseInspect(stdout, c.id)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return execTargetInspection{state: execTargetInspectionFailed, err: contextErr}
	}
	if err != nil {
		var missing *inspectTargetNotFoundError
		if errors.As(err, &missing) && missing.id == c.id {
			return execTargetInspection{state: execTargetNotFound, err: err}
		}
		return execTargetInspection{state: execTargetInspectionFailed, err: err}
	}
	if info.state == StateRunning {
		return execTargetInspection{state: execTargetRunning}
	}
	return execTargetInspection{state: execTargetStopped}
}
