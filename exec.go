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
			if len(v) > cli.MaxStreamOverlap || !cli.StreamValueFits(v) {
				return invalidOption("exec environment variable", "value is too large to redact safely from streamed diagnostics")
			}
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
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (exitCode int, output io.Reader, retErr error) {
	for i, opt := range opts {
		if opt == nil {
			return 0, nil, validationErrorf("Exec", i, "option %d is nil", i)
		}
	}
	if len(cmd) == 0 {
		return 0, nil, validationErrorf("Exec", cmd, "exec: command must not be empty")
	}
	cfg := &execConfig{env: map[string]string{}, timeout: defaultExecTimeout}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return 0, nil, c.redactError(optionBoundaryError(err), execDiagnosticValues(cfg, cmd)...)
		}
	}

	// Exec is a buffered, bounded operation by default. An explicit zero
	// timeout opts out of the library default. A positive custom timeout
	// is combined with the caller's deadline, so the earlier one wins.
	execCtx, cancel := withExecTimeout(ctx, cfg)
	defer cancel()

	// Do not hand an already-finished context to a runner. In particular,
	// exec.CommandContext can otherwise return a plain context error before
	// Start, which must not be reported as an unsupported remote-process
	// termination. Keep the output contract even for this early return.
	if ctxErr := effectiveExecContextErr(ctx, execCtx); ctxErr != nil {
		return 0, emptyExecOutput(), ctxErr
	}

	target, err := c.verifiedOperationTarget(execCtx)
	if err != nil {
		return 0, emptyExecOutput(), err
	}

	var envFile, envDir string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFileContext(execCtx, cfg.env)
		if err != nil {
			if dir != "" {
				// Preserve ownership when a late root-lock error is
				// returned with a published env directory.
				defer func() {
					if retryErr := retryEnvFileCleanupWithError(&dir); retryErr != nil {
						retErr = joinEnvFileCleanupError(retErr, retryErr)
					}
				}()
				return 0, emptyExecOutput(), joinEnvFileCleanupError(err, cleanupEnvFileWithRetry(dir))
			}
			return 0, emptyExecOutput(), err
		}
		envFile, envDir = path, dir
		defer func() {
			if retryErr := retryEnvFileCleanupWithError(&envDir); retryErr != nil {
				retErr = joinEnvFileCleanupError(retErr, retryErr)
			}
		}()
	}
	// Setup (for example an env-file write) may itself race with
	// cancellation. Recheck immediately before handing control to the
	// runner so that path remains a no-launch result.
	if ctxErr := effectiveExecContextErr(ctx, execCtx); ctxErr != nil {
		return 0, emptyExecOutput(), ctxErr
	}

	stdout, stderr, err := c.runner.Run(execCtx, c.eng.execArgs(target, cfg, envFile, cmd)...)
	// The CLI has finished reading the env file. Remove it before any
	// result classification or caller-visible output processing. Retain
	// envDir for a deferred retry if removal fails, and preserve the error.
	envCleanupErr := cleanupEnvFileAfterUseContext(execCtx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}

	// A runner call is considered a launch for legacy/custom runners that
	// do not report lifecycle metadata. ExecRunner annotates failures with
	// the authoritative local Start/reap/cancellation result below.
	launched := true
	commandStatus, statusReported := cli.RunStatusOf(err)
	if statusReported {
		launched = commandStatus.Started
	}
	output = io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	if err == nil {
		return 0, output, envCleanupErr
	}

	// Capture whether cancellation was already present when the command
	// returned. A later deadline consumed by the verification inspect must
	// not be mistaken for a cancellation race with the command itself.
	commandContextErr := isExecContextError(err) || effectiveExecContextErr(ctx, execCtx) != nil
	terminationUnknown := launched && commandContextErr
	if statusReported {
		terminationUnknown = commandStatus.Reaped && commandStatus.TerminatedByCancellation
	}

	// A custom runner may return a plain CLIError when cancellation races
	// with its return. Normalize the effective context into the chain before
	// deciding whether this is a context result.
	if ctxErr := effectiveExecContextErr(ctx, execCtx); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}
	finish := func(code int, classified error) (int, io.Reader, error) {
		if contextErr, ok := execContextualError(ctx, execCtx, classified, terminationUnknown); ok {
			return code, output, joinEnvFileCleanupError(contextErr, envCleanupErr)
		}
		return code, output, joinEnvFileCleanupError(classified, envCleanupErr)
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

	if isExecContextError(err) || effectiveExecContextErr(ctx, execCtx) != nil {
		code := 0
		if cliErr != nil && cliErr.ExitCode >= 0 {
			code = cliErr.ExitCode
		}
		return finish(code, err)
	}
	if cliErr != nil && cliErr.ExitCode < 0 {
		return finish(cliErr.ExitCode, err)
	}

	if cliErr == nil {
		return finish(0, wrapNotFoundForOperation(c.eng, c.classifyOperation(execCtx, err, "exec"), "exec", c.operationTarget(), c.id))
	}

	// A timeout-shaped command exit is an operation failure, not an
	// application result. Classify returns it without probing the backend.
	if isExecOperationTimeout(err, cliErr) {
		return finish(cliErr.ExitCode, c.classifyOperation(execCtx, err, "exec"))
	}

	if cliErr.ExitCode == 126 || cliErr.ExitCode == 127 {
		return finish(cliErr.ExitCode, nil)
	}

	// Exec diagnostics can come from the workload itself. Do not use the
	// broad text-based liveness classifier here: a positive workload exit
	// is still a result even when it says "permission denied" or mentions
	// configuration. Only structured OS and timeout evidence is definitive.
	if isDefinitiveExecError(err, cliErr) {
		return finish(cliErr.ExitCode, err)
	}

	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFoundForOperation(c.eng, err, "exec", c.operationTarget(), c.id) &&
		!maybeInfraExecErr(c.eng, err, c.operationTarget(), c.id) {
		return finish(cliErr.ExitCode, nil)
	}
	verification := c.verifyExecContainer(execCtx)
	if verification.err == nil && verification.state == StateRunning {
		return finish(cliErr.ExitCode, nil)
	}
	if verification.err != nil {
		joined := errors.Join(err, verification.err)
		return finish(cliErr.ExitCode, wrapNotFoundForOperation(c.eng, joined, "exec", c.operationTarget(), c.id))
	}
	return finish(cliErr.ExitCode, errors.Join(err, &execInspectionStateError{state: verification.state}))
}

func emptyExecOutput() io.Reader {
	return io.MultiReader(bytes.NewReader(nil), bytes.NewReader(nil))
}

func isExecContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func effectiveExecContextErr(ctx, execCtx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return execCtx.Err()
}

func execContextualError(ctx, execCtx context.Context, err error, terminationUnknown bool) (error, bool) {
	ctxErr := effectiveExecContextErr(ctx, execCtx)
	if ctxErr == nil && !isExecContextError(err) {
		return nil, false
	}
	if ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}
	err = wrapNotFound(err)
	// The command may have completed before a later diagnostic inspect
	// consumed the deadline. Only a cancellation observed at runner return
	// leaves the remote exec termination unknown.
	if terminationUnknown {
		err = &ExecTerminationError{Err: err}
	}
	return err, true
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
	if cli.IsOperationTimeoutError(err) {
		return true
	}
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
