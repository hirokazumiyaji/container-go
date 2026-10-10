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

// WithExecEnv sets environment variables for the exec'd process, passed via a
// temporary env file. It uses the same key and value validation as WithEnv.
// Exec returns ErrEnvFileUnsupported on Windows.
func WithExecEnv(env map[string]string) ExecOption {
	return func(c *execConfig) error {
		for _, k := range sortedKeys(env) {
			v := env[k]
			if err := validateEnvironmentEntry("WithExecEnv", "exec environment variable", k, v); err != nil {
				return err
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
			return validationErrorf("WithExecUser", u, "invalid exec user %q", u)
		}
		c.user = u
		return nil
	}
}

// WithExecWorkDir sets the working directory for the exec'd process.
func WithExecWorkDir(dir string) ExecOption {
	return func(c *execConfig) error {
		if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\n\x00") {
			return validationErrorf("WithExecWorkDir", dir, "exec working directory %q must be an absolute path", dir)
		}
		c.workdir = dir
		return nil
	}
}

// Exec runs a command in the container and returns its exit code and
// combined output. A non-zero exit code is a result, not an error.
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (exitCode int, output io.Reader, retErr error) {
	for i, opt := range opts {
		if opt == nil {
			return 0, nil, validationErrorf("Exec", i, "option %d is nil", i)
		}
	}
	if len(cmd) == 0 {
		return 0, nil, validationErrorf("Exec", cmd, "exec: command must not be empty")
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

	stdout, stderr, err := c.runner.Run(ctx, c.eng.execArgs(target, cfg, envFile, cmd)...)
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

	var cliErr *cli.CLIError
	hasSelected := false
	if c.eng != nil {
		var selected cliErrorBranch
		selected, hasSelected = matchingCLIErrorBranch(err, c.eng.binary(), "exec", c.operationTarget(), c.id)
		if hasSelected {
			cliErr = selected.ctx.err
		}
	}
	if !hasSelected {
		_ = errors.As(err, &cliErr)
	}

	if isExecContextError(err) || (cliErr != nil && cliErr.ExitCode < 0) || ctx.Err() != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(err, ctxErr)
		}
		code := 0
		if cliErr != nil && cliErr.ExitCode >= 0 {
			code = cliErr.ExitCode
		}
		return code, output, joinEnvFileCleanupError(err, envCleanupErr)
	}
	if cliErr == nil {
		return 0, output, joinEnvFileCleanupError(wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "exec"), "exec", c.operationTarget(), c.id), envCleanupErr)
	}
	// A timeout-shaped command exit is an operation failure, not an
	// application result. Classify returns it without probing the backend.
	if isExecOperationTimeout(err, cliErr) {
		return cliErr.ExitCode, output, joinEnvFileCleanupError(c.classifyOperation(ctx, err, "exec"), envCleanupErr)
	}
	if cliErr.ExitCode == 126 || cliErr.ExitCode == 127 {
		return cliErr.ExitCode, output, envCleanupErr
	}
	// Exec diagnostics can come from the workload itself. Do not use the
	// broad text-based liveness classifier here: a positive workload exit
	// is still a result even when it says "permission denied" or mentions
	// configuration. Only structured OS and timeout evidence is definitive.
	if isDefinitiveExecError(err, cliErr) {
		return cliErr.ExitCode, output, joinEnvFileCleanupError(err, envCleanupErr)
	}
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFoundForOperation(c.eng, err, "exec", c.operationTarget(), c.id) &&
		!maybeInfraExecErr(c.eng, err, c.operationTarget(), c.id) {
		return cliErr.ExitCode, output, envCleanupErr
	}
	verification := c.verifyExecContainer(ctx)
	if verification.err == nil && verification.state == StateRunning {
		return cliErr.ExitCode, output, envCleanupErr
	}
	if verification.err != nil {
		joined := errors.Join(err, verification.err)
		return 0, nil, joinEnvFileCleanupError(wrapNotFoundForOperation(c.eng, joined, "exec", c.operationTarget(), c.id), envCleanupErr)
	}
	return 0, nil, joinEnvFileCleanupError(errors.Join(err, &execInspectionStateError{state: verification.state}), envCleanupErr)
}

func isExecContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isExecOperationTimeout(err error, cliErr *cli.CLIError) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	return cliErr != nil && cliErr.ExitCode < 0
}

// isDefinitiveExecError identifies client-side failures that are
// represented by structured evidence rather than workload output. Exec
// forwards the workload's stderr through CLIError, so diagnostic phrases
// alone cannot establish that the backend or the CLI invocation failed.
func isDefinitiveExecError(err error, cliErr *cli.CLIError) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	return false
}

// maybeInfraExecErr reports whether an exec CLIError could be about the
// execution substrate rather than the app process. Generic app output
// returns false so normal non-zero exits cost no extra probe.
func maybeInfraExecErr(eng engine, err error, targets ...string) bool {
	if eng == nil {
		return false
	}
	for _, branch := range matchingCLIErrorBranches(err, eng.binary(), "exec", targets...) {
		stderr, _ := branchLines(branch, false)
		if len(stderr) == 0 {
			return true
		}
		for _, line := range stderr {
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
	}
	return false
}

type execInspection struct {
	state State
	err   error
}

type execInspectionStateError struct {
	state State
}

type execInspectEmptyError struct {
	target string
}

func (e *execInspectEmptyError) Error() string {
	return fmt.Sprintf("exec verification: empty inspect output for %q", e.target)
}

func (e *execInspectionStateError) Error() string {
	return fmt.Sprintf("exec verification: container state is %s", e.state)
}

// verifyExecContainer returns both the verified state and the precise
// verification error. A false state is not enough to turn a failure into
// ErrContainerNotFound: only a structured backend absence is wrapped as
// such. Parse, empty-output, cancellation, permission, configuration, and
// transport failures remain visible to the caller.
func (c *Container) verifyExecContainer(ctx context.Context) execInspection {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	target := c.operationTarget()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		return execInspection{
			state: StateUnknown,
			err:   wrapNotFoundForOperation(c.eng, c.classifyOperation(qCtx, err, "inspect"), "inspect", target, c.id),
		}
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return execInspection{
			state: StateUnknown,
			err:   &execInspectEmptyError{target: target},
		}
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		if errors.Is(err, errInspectTargetNotFound) {
			return execInspection{state: StateUnknown, err: wrapInspectTargetNotFound(err)}
		}
		return execInspection{state: StateUnknown, err: err}
	}
	if info == nil {
		return execInspection{
			state: StateUnknown,
			err:   fmt.Errorf("exec verification: inspect returned no state"),
		}
	}
	if c.dockerGenerationMatches(info) {
		c.setImmutableUID(info.uid)
	}
	return execInspection{state: info.state}
}
