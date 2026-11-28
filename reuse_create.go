package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func reuseCreateResolved(ctx context.Context, image string, cfg *config, resolvedImage imageIdentity) (result *Container, retErr error) {
	if cfg.creation == "" {
		cfg.creation = newCreationID()
	}

	// The leader's pull and create get an independent runTimeout budget
	// even when the caller's context carries a tighter attach deadline.
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), cfg.effectiveRunTimeout())
	defer cancel()
	materializeRunPlatform(cfg)
	// Keep the secret file out of the image-pull and attach/retry windows;
	// it exists only for the run command that consumes it. A failed first
	// removal is returned and retried by the deferred cleanup.
	var envFile, envDir string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFileContext(runCtx, cfg.env)
		if err != nil {
			if dir != "" {
				// Keep retry ownership when a late root-lock error is
				// returned together with a published env directory.
				defer func() {
					if retryErr := retryEnvFileCleanupWithError(&dir); retryErr != nil {
						retErr = joinEnvFileCleanupError(retErr, retryErr)
					}
				}()
				return nil, joinEnvFileCleanupError(err, cleanupEnvFileWithRetry(dir))
			}
			return nil, err
		}
		envFile, envDir = path, dir
		defer func() {
			if retryErr := retryEnvFileCleanupWithError(&envDir); retryErr != nil {
				retErr = joinEnvFileCleanupError(retErr, retryErr)
			}
		}()
	}

	preRegisterRunWithGlobalReaper(cfg)
	protectReuseReaper(cfg)
	runArgsImage := resolvedImage.reference
	if runArgsImage == "" {
		runArgsImage = image
	}
	stdout, _, attempted, runErr := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, runArgsImage, envFile)...)
	envCleanupErr := cleanupEnvFileAfterUseContext(runCtx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}
	if runErr != nil {
		if !attempted {
			if target, ok := runReaperTarget(cfg); ok {
				_ = unregisterWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
			}
			return nil, joinEnvFileCleanupError(runErr, envCleanupErr)
		}
		classified := classifyErrorForWithProbe(ctx, cfg.runner, runErr, cfg.eng, cfg.probeTimeout, "run", cfg.name)
		if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(runErr) || createRaceMissing(classified) {
			// Keep the backend conflict and cleanup failure as separate
			// causes. A pending env artifact must not be converted into a
			// successful attach during the conflict fallback.
			operationErr := runErr
			if !cfg.eng.nameConflict(runErr) && cfg.eng.nameConflict(classified) {
				operationErr = classified
			}
			if envCleanupErr != nil {
				return nil, newReuseCreateError(operationErr, envCleanupErr)
			}
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			return nil, operationErr
		}
		if cleanupErr := cleanupFailedCreate(ctx, cfg, runErr, classified); cleanupErr != nil {
			return nil, joinEnvFileCleanupError(withCleanupError(classified, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
		}
		return nil, joinEnvFileCleanupError(classified, envCleanupErr)
	}

	uid := cfg.eng.parseRunID(stdout)
	if requiresImmutableID(cfg.eng) && !validImmutableID(cfg.eng, uid) {
		identityErr := identityError(fmt.Sprintf("%s run returned no valid immutable container ID", cfg.eng.name()))
		if cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr); cleanupErr != nil {
			return nil, joinEnvFileCleanupError(withCleanupError(identityErr, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
		}
		return nil, joinEnvFileCleanupError(identityErr, envCleanupErr)
	}
	ctr := &Container{
		id:              cfg.name,
		runner:          cfg.runner,
		eng:             cfg.eng,
		queryTimeout:    cfg.queryTimeout,
		probeTimeout:    cfg.probeTimeout,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		reused:          true,
		created:         true,
		creation:        cfg.creation,
		image:           resolvedImage,
		sync:            &lockPack{uid: uid},
		dockerMeta:      cfg.dockerMeta,
	}
	ctr.applyConfigDiagnostics(cfg)
	var createdInfo *engineInfo
	if target, ok := runReaperTarget(cfg); ok {
		info, err := verifyCreatedOwnership(ctx, ctr, cfg)
		if err != nil {
			protectReuseReaper(cfg)
			cleanupErr := cleanupFailedCreate(ctx, cfg, err, err)
			if cleanupErr != nil {
				return nil, joinEnvFileCleanupError(withCleanupError(err, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
			}
			return nil, joinEnvFileCleanupError(err, envCleanupErr)
		}
		createdInfo = info
		_ = markSharedWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
		unregisterReuseFromGlobalReaper(cfg)
	}
	if envCleanupErr != nil && len(cfg.files) > 0 {
		// Do not perform required post-create setup while the env artifact
		// is still owned by a pending cleanup. A successful retry clears
		// envDir; otherwise the handle is explicitly incomplete and the
		// flight must not be published as ready.
		if retryErr := retryEnvFileCleanupWithError(&envDir); retryErr != nil {
			incomplete := &reuseIncompleteSetupError{err: errors.Join(envCleanupErr, retryErr)}
			if terminateErr := ctr.Terminate(context.WithoutCancel(ctx)); terminateErr != nil {
				return ctr, &reuseIncompleteSetupError{err: errors.Join(incomplete.err, terminateErr)}
			}
			return nil, incomplete
		}
		envCleanupErr = nil
	}
	if envCleanupErr != nil && len(cfg.files) == 0 {
		// The handle is usable, but no further post-create operation may
		// run while the secret artifact remains. The deferred retry keeps
		// ownership and the flight layer carries this as a warning.
		return ctr, &reuseCleanupWarning{err: envCleanupErr}
	}
	if createdInfo == nil {
		// Without an external reaper there was no ownership inspect to
		// reuse, so obtain the post-create generation now.
		inspectCtx, inspectCancel := context.WithTimeout(ctx, cfg.effectiveReuseAttachTimeout())
		defer inspectCancel()
		for {
			var err error
			createdInfo, err = ctr.inspectDynamicFresh(inspectCtx)
			if err == nil {
				break
			}
			if err := inspectRetryError(inspectCtx, err); err != nil {
				return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
			}
			if err := waitForReusePoll(inspectCtx, cfg.effectiveReusePollInterval()); err != nil {
				return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
			}
		}
	}
	if err := attachDefaultNetworkForConfig(ctx, cfg, createdInfo); err != nil {
		return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
	}
	ctr.storeDefaultNetwork(createdInfo.defaultNetwork)
	if err := checkReuseCompatIdentity(createdInfo, resolvedImage, image, cfg); err != nil {
		return nil, joinEnvFileCleanupError(cleanupReuseCreate(ctx, ctr, err), envCleanupErr)
	}
	if err := verifyStrictCreatedImage(context.WithoutCancel(ctx), ctr, resolvedImage, createdInfo); err != nil {
		return nil, joinEnvFileCleanupError(cleanupReuseCreate(ctx, ctr, err), envCleanupErr)
	}
	if err := copyReuseFiles(ctx, ctr, cfg.files); err != nil {
		return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
	}
	if envCleanupErr != nil {
		return ctr, &reuseCleanupWarning{err: envCleanupErr}
	}
	if contextErr := commandContextError(runCtx, nil); contextErr != nil {
		return nil, cleanupReuseCreate(ctx, ctr, contextErr)
	}
	return ctr, nil
}

// cleanupReuseCreate discards a reuse generation whose creation could not be
// completed. Like rollback, CONTAINERGO_KEEP=1 leaves it for debugging, and a
// failed removal is joined as a *CleanupError so callers can detect the
// leftover with errors.Is(err, ErrCleanupFailed).
func cleanupReuseCreate(ctx context.Context, ctr *Container, cause error) error {
	if keepContainers() {
		return cause
	}
	if err := ctr.Terminate(context.WithoutCancel(ctx)); err != nil {
		return withCleanupError(cause, &CleanupError{Container: ctr.id, Err: err})
	}
	return cause
}

func copyReuseFiles(ctx context.Context, ctr *Container, files []File) error {
	if err := ctr.copyFiles(ctx, files); err != nil {
		var fErr *fileCopyError
		if errors.As(err, &fErr) {
			return fmt.Errorf("reuse %s: WithFiles copy %q to %q: %w", ctr.id, fErr.Source, fErr.ContainerPath, fErr.Err)
		}
		return fmt.Errorf("reuse %s: WithFiles copy: %w", ctr.id, err)
	}
	return nil
}

func inspectRetryError(ctx context.Context, err error) error {
	if retryableInspectError(ctx, err) {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func retryableInspectError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, ErrSystemNotRunning) || errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidConfig) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return !cli.IsProbeConfigurationError(err)
	}
	return transientReuseInspectError(err)
}

func transientReuseInspectError(err error) bool {
	if err == nil || errors.Is(err, ErrSystemNotRunning) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	message := err.Error()
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		message = cliErr.Stderr
	}
	message = strings.ToLower(message)
	for _, marker := range []string{
		"temporary", "temporarily", "try again", "timeout", "timed out",
		"temporarily unavailable", "unavailable", "connection", "transport",
		"xpc", "busy", "not ready", "try later", "eof", "reset by peer",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func reuseInspectWaitError(name string, waitErr, lastInspectErr error) error {
	if waitErr == nil {
		return nil
	}
	if lastInspectErr == nil {
		if errors.Is(waitErr, context.DeadlineExceeded) {
			return fmt.Errorf("reuse %s: timed out waiting for a usable container: %w", name, waitErr)
		}
		return waitErr
	}
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container (last inspect error: %w): %w", name, lastInspectErr, waitErr)
	}
	return fmt.Errorf("reuse %s: %w (last inspect error: %w)", name, waitErr, lastInspectErr)
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.waitStrategy == nil {
		return nil
	}
	if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: ctr}); err != nil {
		values := wait.DiagnosticValues(cfg.waitStrategy)
		tail, tailErr := ctr.logTailWithError(context.WithoutCancel(ctx), values...)
		safeErr := ctr.publicError(fmt.Errorf("reuse failed to become ready: %w", err), values...)
		if tailErr != nil {
			return ctr.publicError(fmt.Errorf("%w; container log tail unavailable: %w", safeErr, tailErr), values...)
		}
		if tail != "" {
			return ctr.publicError(fmt.Errorf("%w; container logs: %s", safeErr, tail), values...)
		}
		return safeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func inspectNamed(ctx context.Context, cfg *config, id string) (*engineInfo, error) {
	return namedContainer(cfg, id).inspectFresh(ctx)
}

func inspectReuseBase(ctx context.Context, base *Container, cfg *config) (*engineInfo, error) {
	ctr := namedContainer(cfg, cfg.name)
	if base != nil {
		uid, _, err := base.identitySnapshot(ctx)
		if err != nil {
			return nil, err
		}
		if uid != "" {
			ctr.pack().uid = uid
		}
	}
	return ctr.inspectFresh(ctx)
}

func namedContainer(cfg *config, id string) *Container {
	return &Container{
		id:              id,
		runner:          cfg.runner,
		eng:             cfg.eng,
		queryTimeout:    cfg.queryTimeout,
		probeTimeout:    cfg.probeTimeout,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		nameInspect:     true,
		dockerMeta:      cfg.dockerMeta,
	}
}

// createRaceMissing reports only Apple's anchored concurrent-create form:
// a run command that reaches bootstrap and then loses the named object.
// Generic "container not found" output is deliberately excluded because
// it is commonly emitted by the application process and must remain an
// ordinary original error.
func createRaceMissing(err error) bool {
	return createRaceMissingForTarget(err, "")
}

func createRaceMissingForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	argv, ok := cliCommandParts(err)
	if !ok {
		return false
	}
	command := strings.ToLower(cli.CommandOperation(argv))
	if command != "run" || !cliErrorBelongsTo(err, "container") {
		return false
	}
	cliTarget := commandTarget(argv, command)
	if cliTarget == "" || (target != "" && !strings.EqualFold(cliTarget, target)) {
		return false
	}
	// Prefer the nested ContainerizationError path (issue #118): a run that
	// reaches bootstrap/run and then loses the named object. A bare
	// notFound envelope without that run wrapper is application output.
	if hasAppleContainerizationErrorPath(err, "internalerror", func(message string) bool {
		return message == "failed to bootstrap container" || message == "failed to run container"
	}, "notfound", func(message string) bool {
		return appleIDMissingLine(message, cliTarget)
	}) {
		return true
	}
	return hasCLIErrorLine(err, func(line string) bool {
		if appleIDMissingLine(line, cliTarget) {
			return true
		}
		for _, wrapper := range []string{
			"failed to bootstrap container:",
			"failed to run container:",
		} {
			rest, found := strings.CutPrefix(line, wrapper)
			if found && appleIDMissingLine(strings.TrimSpace(rest), cliTarget) {
				return true
			}
		}
		return false
	})
}
