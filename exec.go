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

	// verifyCtx bounds the identity lock and verification inspect. The user
	// command runs under the caller's ctx: Exec has no default deadline, and
	// a library-injected timeout would be reported as an exit code rather
	// than as the caller's cancellation.
	verifyCtx, verifyCancel := withDefaultTimeout(ctx, queryTimeout)
	defer verifyCancel()

	// The name lock must stay held until the backend has resolved the name,
	// so the command is spawned inside the critical section. Waiting for it
	// under the lock would block every other name-addressed operation (Stop,
	// Terminate, Logs, the reaper) for as long as the app runs, and Exec
	// applies no default deadline to bound that.
	spawner, canSpawn := c.runner.(cli.Spawner)
	var target string
	var started *cli.StartedCommand
	var stdout, stderr []byte
	var runErr error
	if canSpawn {
		err := c.withVerifiedOperationTarget(verifyCtx, true, func(t string, _ *engineInfo) error {
			child, err := spawner.Start(ctx, c.eng.execArgs(t, cfg, envFile, cmd)...)
			if err != nil {
				return wrapNotFoundFor(c.eng, c.classify(ctx, err))
			}
			target, started = t, child
			return nil
		})
		if err != nil {
			return 0, nil, err
		}
		stdout, stderr, runErr = started.Wait()
	} else {
		// Without a spawner there is no way to release the lock between the
		// name resolution and the child process, so the command has to run
		// inside the critical section. Holding the name lock for an unbounded
		// command blocks other name-addressed operations, but releasing it
		// early would let a peer delete and recreate the name first, and the
		// command would run in a different container.
		err := c.withVerifiedOperationTarget(ctx, true, func(t string, _ *engineInfo) error {
			target = t
			stdout, stderr, runErr = c.runner.Run(ctx, c.eng.execArgs(t, cfg, envFile, cmd)...)
			return nil
		})
		if err != nil {
			return 0, nil, err
		}
	}
	output := io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr))
	if runErr == nil {
		return 0, output, nil
	}
	if !cli.IsCommandExit(runErr) {
		return 0, output, wrapNotFoundFor(c.eng, c.classify(ctx, runErr))
	}
	var cliErr *cli.CLIError
	errors.As(runErr, &cliErr)
	exitCode := cliErr.ExitCode
	// App stderr alone must not decide infrastructure state. Only
	// ambiguous failures pay for a verification inspect; clear app
	// results return immediately with no extra CLI call.
	if !isNotFoundFor(c.eng, runErr) && !maybeInfraExecErr(runErr) {
		return exitCode, output, nil
	}
	// Bound the re-check from the caller's context: verifyCtx is already
	// spent once a long-running command returns.
	if c.execContainerRunningTarget(ctx, target) {
		return exitCode, output, nil
	}
	return exitCode, output, wrapNotFoundFor(c.eng, c.classify(ctx, runErr))
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
	stdout, stderr, ok := cli.DiagnosticText(err)
	if !ok {
		return "", false
	}
	return strings.ToLower(stdout + "\n" + stderr), true
}

// execContainerRunningTarget reports whether the operation target is still
// running, as a best-effort liveness probe for an ambiguous exec failure.
//
// It does not hold the name lock: the command is spawned inside the
// critical section and then runs unbounded, so the lock is released before
// this call. A name-addressed target can therefore be replaced in between,
// so the creation generation is re-checked here. A probe that cannot prove
// "the same container is running" returns false, which keeps the ambiguous
// failure classified as an error rather than as an app result.
func (c *Container) execContainerRunningTarget(ctx context.Context, target string) bool {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		return false
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		return false
	}
	if !validCreationID(c.creation) || info.labels[creationLabel] != c.creation {
		return false
	}
	if c.uid != "" && requiresImmutableID(c.eng) && info.uid != c.uid {
		return false
	}
	return info.state == StateRunning
}
