package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// reuseFlights collapses concurrent WithReuse get-or-create calls that
// share a name into one ensure operation. Each caller still applies its
// own compatibility check and wait strategy afterward.
var reuseFlights flightGroup[*Container]

type reuseCleanupWarning struct {
	err error
}

func (w *reuseCleanupWarning) Error() string { return w.err.Error() }
func (w *reuseCleanupWarning) Unwrap() error { return w.err }

func (w *reuseCleanupWarning) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**reuseCleanupWarning)
	if !ok || w == nil {
		return false
	}
	*ptr = &reuseCleanupWarning{err: cli.WithRedactor(w.err, r)}
	return true
}

func isReuseCleanupWarning(err error) bool {
	var warning *reuseCleanupWarning
	return errors.As(err, &warning)
}

type reuseCreateError struct {
	operation error
	cleanup   error
}

func (e *reuseCreateError) Error() string {
	return errors.Join(e.operation, e.cleanup).Error()
}

func (e *reuseCreateError) Unwrap() []error {
	return []error{e.operation, e.cleanup}
}

func (e *reuseCreateError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**reuseCreateError)
	if !ok || e == nil {
		return false
	}
	*ptr = &reuseCreateError{operation: cli.WithRedactor(e.operation, r), cleanup: cli.WithRedactor(e.cleanup, r)}
	return true
}

func newReuseCreateError(operation, cleanup error) error {
	if cleanup == nil {
		return operation
	}
	return &reuseCreateError{operation: operation, cleanup: cleanup}
}

func reuseCreateOperationError(err error) error {
	var createErr *reuseCreateError
	if errors.As(err, &createErr) && createErr.operation != nil {
		return createErr.operation
	}
	return err
}

func reuseCreateCleanupError(err error) error {
	var createErr *reuseCreateError
	if errors.As(err, &createErr) {
		return createErr.cleanup
	}
	return nil
}

type reuseIncompleteSetupError struct {
	err error
}

func (e *reuseIncompleteSetupError) Error() string { return e.err.Error() }
func (e *reuseIncompleteSetupError) Unwrap() error { return e.err }

func (e *reuseIncompleteSetupError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**reuseIncompleteSetupError)
	if !ok || e == nil {
		return false
	}
	*ptr = &reuseIncompleteSetupError{err: cli.WithRedactor(e.err, r)}
	return true
}
func isReuseIncompleteSetupError(err error) bool {
	var incomplete *reuseIncompleteSetupError
	return errors.As(err, &incomplete)
}

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// PullAlways is a per-caller side effect, even when the container is
	// already running. Do it directly before joining the shared ensure
	// flight so every caller performs its own fetch instead of being
	// absorbed by an in-flight image pull.
	if cfg.pullPolicy == PullAlways {
		pullCtx, cancel := withDefaultTimeout(ctx, runTimeout)
		defer cancel()
		if err := pullImage(pullCtx, cfg.runner, cfg.eng, image, cfg.platform); err != nil {
			return nil, err
		}
		cfg.imagePrepared = true
	}
	key := cfg.eng.targetID() + "\x00" + cfg.name
	var leaderWarning error
	base, leader, err := reuseFlights.doWithLeader(ctx, key, func() (*Container, error) {
		// Do not detach a canceled preflight into a new shared flight.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ctr, ensureErr := reuseEnsureContainer(flightCtx, image, cfg)
		if ctr != nil && isReuseCleanupWarning(ensureErr) {
			// A cleanup warning belongs to the creator's returned handle,
			// but it is a successful shared create. Do not make every
			// waiter observe a failed flight.
			leaderWarning = ensureErr
			return ctr, nil
		}
		return ctr, ensureErr
	})
	if err != nil {
		// A post-create result can carry a real error together with a
		// usable handle. Only the flight leader may receive that handle;
		// waiters must retry/attach and perform their own checks.
		if leader && base != nil {
			return base, err
		}
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if base == nil {
		return nil, errors.New("reuse returned an empty container handle")
	}

	// The ensure result may have been inspected before a peer changed
	// network or port state. Once Docker has supplied an immutable ID,
	// every compatibility inspect must target that ID rather than the
	// The shared ensure may have been started with another caller's image
	// reference; every caller must still verify that the container is
	// compatible with its own resolved image identity.
	resolvedImage, err := cfg.ensureImageRef(ctx, image)
	if err != nil {
		return nil, err
	}

	// The ensure result may have been inspected before a peer changed
	// network or port state. Once Docker has supplied an immutable ID,
	// every compatibility inspect must target that ID rather than the
	// reusable name.
	infoCtx, infoCancel := context.WithTimeout(ctx, reuseAttachTimeout)
	defer infoCancel()
	info, err := reuseInfoForCaller(infoCtx, cfg, base)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("reuse %s: timed out waiting for complete inspect: %w", cfg.name, err)
		}
		if leader && leaderWarning != nil {
			// The create itself succeeded and the only known warning is
			// cleanup. Preserve that usable handle even if the fresh
			// verification needed for compatibility cannot complete.
			return base, joinEnvFileCleanupError(err, leaderWarning)
		}
		return nil, err
	}
	if err := attachDefaultNetworkForConfig(ctx, cfg, info); err != nil {
		return nil, err
	}
	if err := checkReuseCompatIdentity(info, resolvedImage, image, cfg); err != nil {
		return nil, joinEnvFileCleanupError(err, leaderWarning)
	}

	if cfg.eng.name() == "apple" && resolvedImage.pinned {
		// A pinned reuse run requires an existing container to be
		// running the requested root digest. Docker enforces this via
		// inspect; Apple uses the same post-bootstrap probe on fresh
		// reuse adoption that verifyAppleCreatedImage closes for create.
		probe := namedContainer(cfg, cfg.name)
		probe.image = resolvedImage
		if err := verifyAppleCreatedImage(ctx, probe, resolvedImage); err != nil {
			return nil, err
		}
	}

	containerImage := imageFromInfo(info)
	if resolvedImage.pinned {
		containerImage = resolvedImage
	}

	ctr := &Container{
		id:              base.id,
		runner:          cfg.runner,
		eng:             cfg.eng,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		reused:          true,
		creation:        info.labels[creationLabel],
		image:           containerImage,
		sync:            &lockPack{uid: info.uid, info: immutableInfo(info), defaultNetwork: info.defaultNetwork},
	}
	// The leader's files are copied in reuseCreate. Every attaching
	// caller applies its own files after the shared generation is known.
	// A failed attach copy is reported without deleting the shared
	// container, which may be serving other callers.
	if !cfg.reusedCreated {
		if err := copyReuseFiles(ctx, ctr, cfg.files); err != nil {
			return nil, err
		}
	}
	// The creator stops before readiness work when its env artifact is
	// still present. Waiters that received the successful shared result
	// still run their own compatibility/readiness path below.
	if leader && leaderWarning != nil {
		return ctr, leaderWarning
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, joinEnvFileCleanupError(err, leaderWarning)
	}

	// A readiness wait can outlive the generation returned by the
	// ensure. Re-inspect by the immutable ID and repeat compatibility checks
	// before exposing the handle.
	fresh, err := ctr.inspectDynamicFresh(ctx)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
		}
		return nil, err
	}
	if fresh.state != StateRunning {
		return nil, fmt.Errorf("reuse %s: state changed to %s before return", cfg.name, fresh.state)
	}
	if !sameEnginePlatform(info, fresh) {
		return nil, fmt.Errorf("reuse %s: platform changed from %q to %q before return", cfg.name, info.platform, fresh.platform)
	}
	if err := checkReuseOwnedIdentity(fresh, resolvedImage, image, cfg); err != nil {
		return nil, err
	}
	if err := checkReuseCompatIdentity(fresh, resolvedImage, image, cfg); err != nil {
		return nil, err
	}
	return ctr, nil
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller wait or port compatibility checks. Those run in
// reuseRun so every concurrent caller applies its own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false
	var lastInspectErr error
	var resolvedImage imageIdentity
	imageResolved := false
	resolveImage := func() error {
		if imageResolved {
			return nil
		}
		var err error
		resolvedImage, err = cfg.ensureImageRef(context.WithoutCancel(ctx), image)
		if err != nil {
			return err
		}
		imageResolved = true
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if !isNotFoundFor(cfg.eng, err) {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
				}
				if !transientReuseInspectError(err) {
					return nil, err
				}
				lastInspectErr = err
				if err := waitForReusePoll(ctx); err != nil {
					return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
				}
				continue
			}
			lastInspectErr = nil
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			// Resolve the caller's image immediately before create: a
			// tag might have changed while waiting, and we want the
			// immutable image that will actually be passed to run.
			if err := resolveImage(); err != nil {
				return nil, err
			}
			ctr, createErr := reuseCreateResolved(context.WithoutCancel(ctx), image, cfg, resolvedImage)
			if ctr != nil {
				// reuseCreate can return a usable handle together with a
				// post-create cleanup error. Do not turn that into an
				// orphaned container by treating it as a failed create.
				cfg.reusedCreated = true
				return ctr, createErr
			}
			// A name conflict may be attachable only after the env artifact
			// has been cleaned. While cleanup ownership is still pending,
			// preserve both errors and do not enter the attach path.
			if reuseCreateCleanupError(createErr) != nil {
				return nil, createErr
			}
			operationErr := reuseCreateOperationError(createErr)
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Re-inspect and attach (or recreate) until the deadline.
			if cfg.eng.nameConflict(operationErr) || createRaceMissing(operationErr) {
				if err := waitForReusePoll(ctx); err != nil {
					return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
				}
				continue
			}
			return nil, createErr
		}
		lastInspectErr = nil

		if err := resolveImage(); err != nil {
			return nil, err
		}
		if err := checkReuseOwnedIdentity(info, resolvedImage, image, cfg); err != nil {
			return nil, err
		}
		switch info.state {
		case StateCreated, StateRestarting, StateUnknown:
			if err := waitForReusePoll(ctx); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			continue
		case StateStopping:
			return nil, fmt.Errorf("reuse %s: container is stopping and cannot become ready", cfg.name)
		case StatePaused:
			return nil, fmt.Errorf("reuse %s: container is paused and cannot become ready", cfg.name)
		case StateStopped:
			if recreated {
				return nil, fmt.Errorf("reuse: container stayed stopped after recreate")
			}
			// Only recycle containers this library created for reuse
			// with a compatible resolved image; never delete foreign leftovers.
			if err := resolveImage(); err != nil {
				return nil, err
			}
			if err := checkReuseOwnedIdentity(info, resolvedImage, image, cfg); err != nil {
				return nil, err
			}
			if err := deleteStoppedReuse(ctx, cfg, info); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case StateRunning:
			if err := resolveImage(); err != nil {
				return nil, err
			}
			return &Container{
				id:              cfg.name,
				runner:          cfg.runner,
				eng:             cfg.eng,
				exposed:         cfg.exposed,
				published:       cfg.published,
				network:         cfg.network,
				networkExplicit: cfg.networkExplicit,
				reused:          true,
				creation:        info.labels[creationLabel],
				image:           resolvedImage,
				sync:            &lockPack{uid: info.uid, info: immutableInfo(info)},
			}, nil
		default:
			if err := waitForReusePoll(ctx); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
		}
	}
}

func reuseCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	resolvedImage, err := cfg.ensureImageRef(runCtx, image)
	if err != nil {
		return nil, err
	}
	return reuseCreateResolved(ctx, image, cfg, resolvedImage)
}

func reuseCreateResolved(ctx context.Context, image string, cfg *config, resolvedImage imageIdentity) (result *Container, retErr error) {
	if cfg.creation == "" {
		cfg.creation = newCreationID()
	}

	// The leader's pull and create get an independent runTimeout budget
	// even when the caller's context carries a tighter attach deadline.
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	materializeAppleRunPlatform(cfg)
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
		classified := classifyErrorFor(ctx, cfg.runner, runErr, cfg.eng, "run", cfg.name)
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
		identityErr := identityError("Docker run returned no valid immutable container ID")
		if cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr); cleanupErr != nil {
			return nil, joinEnvFileCleanupError(withCleanupError(identityErr, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
		}
		return nil, joinEnvFileCleanupError(identityErr, envCleanupErr)
	}
	ctr := &Container{
		id:              cfg.name,
		runner:          cfg.runner,
		eng:             cfg.eng,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		reused:          true,
		creation:        cfg.creation,
		image:           resolvedImage,
		sync:            &lockPack{uid: uid},
	}
	if target, ok := runReaperTarget(cfg); ok {
		if err := verifyCreatedOwnership(ctx, ctr, cfg); err != nil {
			protectReuseReaper(cfg)
			cleanupErr := cleanupFailedCreate(ctx, cfg, err, err)
			if cleanupErr != nil {
				return nil, joinEnvFileCleanupError(withCleanupError(err, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
			}
			return nil, joinEnvFileCleanupError(err, envCleanupErr)
		}
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
	inspectCtx, inspectCancel := context.WithTimeout(ctx, reuseAttachTimeout)
	defer inspectCancel()
	var createdInfo *engineInfo
	for {
		var err error
		createdInfo, err = ctr.inspectDynamicFresh(inspectCtx)
		if err == nil {
			break
		}
		if !transientReuseInspectError(err) {
			return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
		}
		if err := waitForReusePoll(inspectCtx); err != nil {
			return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
		}
	}
	if err := attachDefaultNetworkForConfig(ctx, cfg, createdInfo); err != nil {
		return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
	}
	ctr.storeDefaultNetwork(createdInfo.defaultNetwork)
	if err := checkReuseCompatIdentity(createdInfo, resolvedImage, image, cfg); err != nil {
		return nil, joinEnvFileCleanupError(cleanupReuseCreate(ctx, ctr, err), envCleanupErr)
	}
	if err := verifyAppleCreatedImage(context.WithoutCancel(ctx), ctr, resolvedImage); err != nil {
		return nil, joinEnvFileCleanupError(cleanupReuseCreate(ctx, ctr, err), envCleanupErr)
	}
	if err := copyReuseFiles(ctx, ctr, cfg.files); err != nil {
		return nil, joinEnvFileCleanupError(ctr.rollback(ctx, err), envCleanupErr)
	}
	if envCleanupErr != nil {
		return ctr, &reuseCleanupWarning{err: envCleanupErr}
	}
	if contextErr := commandContextError(runCtx, nil); contextErr != nil {
		if cleanupErr := ctr.Terminate(context.WithoutCancel(ctx)); cleanupErr != nil {
			return nil, errors.Join(contextErr, fmt.Errorf("cleanup reused failed create: %w", cleanupErr))
		}
		return nil, contextErr
	}
	return ctr, nil
}

func cleanupReuseCreate(ctx context.Context, ctr *Container, cause error) error {
	if err := ctr.Terminate(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (container %s left behind: %v)", cause, ctr.id, err)
	}
	return cause
}

func copyReuseFiles(ctx context.Context, ctr *Container, files []File) error {
	for _, f := range files {
		if err := ctr.copyToContainer(ctx, f.HostPath, f.ContainerPath, false); err != nil {
			return fmt.Errorf("reuse %s: WithFiles copy %q to %q: %w", ctr.id, f.HostPath, f.ContainerPath, err)
		}
	}
	return nil
}

// deleteStoppedReuse removes a stopped reuse container only after a
// fresh ownership/state check. Docker binds the delete to the original
// immutable UID; Apple holds the cooperating-process name lock across
// the fresh inspect and delete. A same-generation running replacement is
// therefore never deleted.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	deleted, err := deleteStoppedReuseResult(ctx, cfg, info)
	if err != nil {
		return err
	}
	if !deleted {
		return nil
	}
	return nil
}

func deleteStoppedReuseResult(ctx context.Context, cfg *config, info *engineInfo) (bool, error) {
	if info == nil {
		return false, fmt.Errorf("reuse %s: missing inspected container", cfg.name)
	}
	labels := info.labels
	dockerLegacy := cfg.eng.name() == "docker" && labels[managedLabel] == "" && labels[reuseLabel] == ""
	if labels[reuseLabel] != "true" && !dockerLegacy {
		return false, nil
	}
	if labels[reuseLabel] == "true" && labels[managedLabel] != "true" {
		return false, fmt.Errorf("reuse %s: managed ownership label is missing", cfg.name)
	}
	creation := labels[creationLabel]
	if !validCreationID(creation) {
		return false, fmt.Errorf("reuse %s: creation ownership label is missing or invalid", cfg.name)
	}
	if info.state != StateStopped {
		return false, nil
	}
	if cfg.eng.name() == "docker" && !validImmutableID(cfg.eng, info.uid) {
		return false, fmt.Errorf("reuse %s: stopped container has no verified immutable ID", cfg.name)
	}

	target := cfg.name
	guardCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	unlock := func() {}
	if !requiresImmutableID(cfg.eng) {
		var err error
		unlock, err = lockName(guardCtx, cfg.name)
		if err != nil {
			return false, fmt.Errorf("reuse %s: lock name: %w", cfg.name, err)
		}
	}
	defer unlock()

	freshContainer := namedContainer(cfg, cfg.name)
	fresh, err := freshContainer.inspectFreshLocked(guardCtx)
	if isNotFoundFor(cfg.eng, err) {
		unregisterContainerReaper(cfg, cfg.name, creation, target)
		return false, nil
	}
	if err != nil {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
	}
	if !sameStoppedReuseGeneration(info, fresh) {
		unregisterContainerReaper(cfg, cfg.name, creation, info.uid)
		return false, nil
	}
	if fresh.state != StateStopped {
		protectReuseReaper(cfg)
		return false, nil
	}
	if cfg.eng.name() == "docker" {
		if !validImmutableID(cfg.eng, fresh.uid) || fresh.uid != info.uid {
			unregisterContainerReaper(cfg, cfg.name, creation, info.uid)
			return false, nil
		}
		target = info.uid
	} else if fresh.uid != "" {
		unregisterContainerReaper(cfg, cfg.name, creation, "")
		return false, fmt.Errorf("reuse %s: Apple inspect returned an unexpected immutable ID", cfg.name)
	}
	_, _, err = cfg.runner.Run(guardCtx, cfg.eng.deleteArgs(target)...)
	if err != nil && !isNotFoundFor(cfg.eng, err) {
		return false, fmt.Errorf("reuse %s: delete stopped generation: %w", cfg.name, err)
	}
	unregisterContainerReaper(cfg, cfg.name, creation, target)
	return true, nil
}

func sameStoppedReuseGeneration(before, after *engineInfo) bool {
	if before == nil || after == nil {
		return false
	}
	if before.labels[managedLabel] != after.labels[managedLabel] ||
		before.labels[reuseLabel] != after.labels[reuseLabel] ||
		before.labels[reuseGroupLabel] != after.labels[reuseGroupLabel] ||
		before.labels[sessionLabel] != after.labels[sessionLabel] ||
		before.labels[creationLabel] != after.labels[creationLabel] {
		return false
	}
	return after.state == StateStopped
}

func rollbackReuse(ctx context.Context, ctr *Container, cfg *config) (bool, error) {
	if ctr == nil {
		return false, errors.New("reuse: missing container for rollback")
	}
	info := ctr.infoSnapshot()
	if info == nil {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: missing inspected generation for rollback", cfg.name)
	}
	if info.state != StateStopped {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: %s generation may be shared; refusing automatic deletion", cfg.name, info.state)
	}
	deleted, err := deleteStoppedReuseResult(ctx, cfg, info)
	if err != nil {
		protectReuseReaper(cfg)
		return false, err
	}
	if !deleted {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: stopped generation changed; refusing automatic deletion", cfg.name)
	}
	return true, nil
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
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		nameInspect:     true,
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
	command, args, ok := cliCommandParts(err)
	if !ok || command != "run" || !cliErrorBelongsTo(err, "container") {
		return false
	}
	cliTarget := cliCommandTarget(command, args)
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

// checkReuseIdentity verifies the ownership facts required before a
// reuse attach or destructive recycle. A reuse marker alone is not proof:
// the managed marker, a valid generation, and (for Docker) a full UID are
// all required.
func checkReuseIdentity(info *engineInfo, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: inspect returned no container identity", cfg.name)
	}
	if info.labels[managedLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container is not managed by container-go", cfg.name)
	}
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse: existing container was not created with WithReuse")
	}
	if !validCreationID(info.labels[creationLabel]) {
		return identityError(fmt.Sprintf("reuse %s: existing container has no valid creation generation", cfg.name))
	}
	if requiresImmutableID(cfg.eng) {
		if !validImmutableID(cfg.eng, info.uid) {
			return identityError(fmt.Sprintf("reuse %s: existing container has no valid immutable ID", cfg.name))
		}
	} else if info.uid != "" {
		return identityError(fmt.Sprintf("reuse %s: name-addressed container unexpectedly has an immutable ID", cfg.name))
	}
	return nil
}

// checkReuseOwnedIdentity reports whether a stopped container may be
// deleted and recreated for this reuse request. The requested image is
// the identity resolved immediately before the create/attach decision,
// not the caller's mutable tag.
func checkReuseOwnedIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if err := checkReuseIdentity(info, cfg); err != nil {
		return err
	}
	if requested.pinned {
		actual := imageFromInfo(info)
		if !requestedImageIdentitiesCompatible(requested, actual) {
			return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, original, info.image)
		}
		return nil
	}
	compatible := imagesCompatible(original, info.image)
	if cfg.eng != nil && cfg.eng.name() == "apple" {
		compatible = appleImageReferencesCompatible(original, info.image)
	}
	if !compatible {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, original, info.image)
	}
	return nil
}

// checkReuseOwned reports whether an existing container may be adopted
// or recycled for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	return checkReuseOwnedIdentity(info, imageIdentity{reference: image}, image, cfg)
}

// checkReuseCompatIdentity applies the resolved image and port checks
// for a reuse request.
func checkReuseCompatIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if err := checkReuseOwnedIdentity(info, requested, original, cfg); err != nil {
		return err
	}
	if cfg.eng != nil && cfg.eng.name() == "docker" {
		requested := cfg.network
		if !cfg.networkExplicit {
			requested = ""
		}
		if err := dockerNetworkModeErrorDefault(requested, info.networkMode, info.networkNames, info.defaultNetwork); err != nil {
			return fmt.Errorf("reuse %s: %w", cfg.name, err)
		}
		if len(cfg.exposed) > 0 || len(cfg.published) > 0 {
			if err := dockerNetworkEndpointError(info.networkMode); err != nil {
				return fmt.Errorf("reuse %s: %w", cfg.name, err)
			}
		}
	}
	// Auto-published exposed ports only appear as host bindings on
	// published-port backends. Explicit WithPublishedPort always needs
	// validation, including on direct-IP engines.
	if !cfg.eng.directIP() {
		for _, spec := range cfg.exposed {
			b, ok := matchingExposedBinding(info.bound, spec, cfg.eng)
			if !ok {
				return fmt.Errorf("reuse %s: exposed port %s missing on existing container", cfg.name, spec)
			}
			if err := checkReuseBindingReachable(cfg.eng, b); err != nil {
				return fmt.Errorf("reuse %s: exposed port %s: %w", cfg.name, spec, err)
			}
		}
	}
	for _, p := range cfg.published {
		b, ok := matchingPublishedBinding(info.bound, p, cfg.eng)
		if !ok {
			return fmt.Errorf("reuse %s: published port %s missing on existing container", cfg.name, p.raw)
		}
		if err := checkReuseBindingReachable(cfg.eng, b); err != nil {
			return fmt.Errorf("reuse %s: published port %s: %w", cfg.name, p.raw, err)
		}
	}
	if cfg.platform != "" {
		if info.platform == "" || platformSelectorUnverifiable(cfg.platform, info.platform) {
			return fmt.Errorf("reuse %s: platform %q could not be verified", cfg.name, cfg.platform)
		}
		if !enginePlatformCompatible(cfg.eng, cfg.platform, info) {
			return fmt.Errorf("reuse %s: platform %q does not match existing %q", cfg.name, cfg.platform, info.platform)
		}
	}
	return nil
}

func checkReuseCompat(info *engineInfo, image string, cfg *config) error {
	return checkReuseCompatIdentity(info, imageIdentity{reference: image}, image, cfg)
}

// imageFromInfo retains the identity reported while inspecting a
// container. When Docker has recorded an immutable repo digest (or ID)
// that matches an index root, imageFromInfo recovers the pinned
// identity that reuse can compare without consulting the daemon again.
func imageFromInfo(info *engineInfo) imageIdentity {
	if info == nil {
		return imageIdentity{}
	}
	platform := info.platform
	variantDigest := info.imageVariantDigest
	imageID := info.imageID
	if canonicalID, ok := canonicalDockerImageID(imageID); ok {
		imageID = canonicalID
	}
	if validImageDigest(info.imageDigest) {
		reference := info.image
		if imageReferenceBase(reference) == "" {
			reference = imageID
		}
		if imageReferenceBase(reference) == "" && isImageID(imageID) {
			return imageIdentity{
				reference:     imageID,
				digest:        info.imageDigest,
				rootDigest:    info.imageDigest,
				repository:    "",
				id:            imageID,
				pinned:        true,
				platform:      platform,
				variantDigest: variantDigest,
			}
		}
		if base := imageReferenceBase(reference); base != "" {
			// Container inspect may report a tag plus a separate
			// configuration descriptor digest. Synthesize the same
			// index@digest reference used for a new run so reuse compares
			// the root content rather than a mutable tag spelling.
			synthesized := base + "@" + info.imageDigest
			if !imageRE.MatchString(synthesized) {
				return imageIdentity{}
			}
			return imageIdentity{
				reference:     synthesized,
				digest:        info.imageDigest,
				rootDigest:    info.imageDigest,
				repository:    imageRepository(base),
				id:            imageID,
				pinned:        true,
				platform:      platform,
				variantDigest: variantDigest,
			}
		}
		// A digest without repository provenance is not a safe identity.
		return imageIdentity{}
	}
	if isImageID(imageID) {
		return imageIdentity{reference: imageID, id: imageID, pinned: true, platform: platform, variantDigest: variantDigest}
	}
	if digest := imageDigest(info.image); validImageDigest(digest) && imageReferenceBase(info.image) != "" {
		return imageIdentity{reference: info.image, digest: digest, rootDigest: digest, repository: imageRepository(info.image), pinned: true, platform: platform, variantDigest: variantDigest}
	}
	// A bare sha256:... in a container's image field is not enough to
	// establish a Docker local ID. Only info.imageID above is verified
	// backend identity data.
	if info.image != "" && !isBareImageReference(info.image) {
		return imageIdentity{reference: info.image, repository: imageRepository(info.image), platform: platform, variantDigest: variantDigest}
	}
	return imageIdentity{}
}

func checkReuseBindingReachable(eng engine, b boundPort) error {
	if eng.name() != "docker" {
		return nil
	}
	_, err := dockerBindingConnectHost(b, eng)
	return err
}

func matchingExposedBinding(bound []boundPort, spec portSpec, eng engine) (boundPort, bool) {
	var loopback boundPort
	foundLoopback := false
	remote := eng != nil && eng.isRemote()
	for _, b := range bound {
		if b.containerPort != spec.port || b.proto != spec.proto || b.hostPort <= 0 {
			continue
		}
		if remote && ipIsLoopback(b.hostAddr) {
			loopback, foundLoopback = b, true
			continue
		}
		return b, true
	}
	return loopback, foundLoopback
}

func matchingPublishedBinding(bound []boundPort, p publishSpec, eng engine) (boundPort, bool) {
	var loopback boundPort
	foundLoopback := false
	remote := eng != nil && eng.isRemote()
	for _, b := range bound {
		if b.containerPort != p.containerPort || b.proto != p.proto || b.hostPort <= 0 {
			continue
		}
		if p.hostPort != 0 && b.hostPort != p.hostPort {
			continue
		}
		if p.hostAddr != "" && canonicalIP(b.hostAddr) != p.hostAddr {
			continue
		}
		if remote && ipIsLoopback(b.hostAddr) {
			loopback, foundLoopback = b, true
			continue
		}
		return b, true
	}
	return loopback, foundLoopback
}

// imagesCompatible reports whether a requested image reference matches
// what inspect reported. Short Docker Hub names are normalized
// (docker.io/library/..., latest) before comparison; arbitrary
// registry/namespace suffix matches are rejected. When the request pins
// a digest, both the normalized base and the digest must match.
func imagesCompatible(requested, actual string) bool {
	if requested == "" || actual == "" {
		return requested == actual
	}
	// A bare digest/ID has no repository namespace. Comparing it by its
	// digest alone would make unrelated registries interchangeable.
	if isBareImageReference(requested) || isBareImageReference(actual) {
		return false
	}
	if requested == actual {
		return true
	}
	reqDigest := imageDigest(requested)
	actDigest := imageDigest(actual)
	if reqDigest != "" {
		if !strings.EqualFold(reqDigest, actDigest) {
			return false
		}
		reqBase := imageReferenceBase(requested)
		actBase := imageReferenceBase(actual)
		return reqBase != "" && actBase != "" &&
			normalizeImageRef(reqBase) == normalizeImageRef(actBase)
	}
	req := imageReferenceBase(requested)
	act := imageReferenceBase(actual)
	return req != "" && act != "" && normalizeImageRef(req) == normalizeImageRef(act)
}

// imageRepository returns the canonical repository portion of an image
// reference, discarding any tag or digest. It is used for a digest
// response that has no tag but still must not cross a registry or
// namespace boundary. A bare ID/digest has no repository and returns
// the empty string.
func imageRepository(ref string) string {
	base := imageReferenceBase(ref)
	if base == "" {
		return ""
	}
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		base = base[:i]
	}
	return strings.TrimSuffix(normalizeImageRef(base), ":latest")
}

// normalizeImageRef expands Docker Hub short names to a canonical
// registry/repo:tag form. The default registry is docker.io, the
// default namespace for single-component repos is library, and the
// default tag is latest.
func normalizeImageRef(ref string) string {
	tag := "latest"
	name := ref
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
		if tag == "" {
			tag = "latest"
		}
	}
	var registry, repo string
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 1:
		registry = "docker.io"
		repo = "library/" + parts[0]
	case len(parts) == 2 && !isRegistry(parts[0]):
		registry = "docker.io"
		repo = name
	case isRegistry(parts[0]):
		registry = canonicalDockerHubRegistry(parts[0])
		repo = strings.Join(parts[1:], "/")
		if registry == "docker.io" && !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	default:
		registry = "docker.io"
		repo = name
	}
	if repo == "" {
		repo = name
	}
	return registry + "/" + repo + ":" + tag
}

func isRegistry(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":") || s == "localhost"
}

func canonicalDockerHubRegistry(registry string) string {
	switch strings.ToLower(registry) {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "docker.io"
	default:
		return registry
	}
}

func isUnqualifiedImageReference(ref string) bool {
	base := imageReferenceBase(ref)
	if base == "" {
		return false
	}
	base = imageRepositoryBaseWithoutTag(base)
	first, _, _ := strings.Cut(base, "/")
	return !isRegistry(first)
}

func isDockerRegistryReference(ref string) bool {
	base := imageReferenceBase(ref)
	if base == "" {
		return false
	}
	base = imageRepositoryBaseWithoutTag(base)
	first, _, _ := strings.Cut(base, "/")
	return canonicalDockerHubRegistry(first) == "docker.io"
}

func imageRepositoryBaseWithoutTag(base string) string {
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		return base[:i]
	}
	return base
}

// imageRepositoryPathsCompatible compares the repository path and tag
// while allowing one side to be an unqualified backend-default name. It is
// intentionally separate from imagesCompatible: the latter retains the
// historical Docker-Hub-only policy for arbitrary public comparisons, while
// Apple inspect responses may legitimately contain a custom default
// registry.
func imageRepositoryPathsCompatible(a, b string) bool {
	aName, aTag, aExplicit := imageRepositoryParts(a)
	bName, bTag, bExplicit := imageRepositoryParts(b)
	if aName == "" || bName == "" {
		return false
	}
	if aExplicit && bExplicit {
		ra, rb := imageRegistryOf(a), imageRegistryOf(b)
		if ra == "" || rb == "" || !strings.EqualFold(ra, rb) {
			return false
		}
	}
	if !tagsCompatible(aTag, bTag) {
		return false
	}
	if aExplicit && bExplicit {
		return aName == bName
	}
	if aExplicit {
		return repositoryNameMatches(bName, aName)
	}
	if bExplicit {
		return repositoryNameMatches(aName, bName)
	}
	return normalizeImageRef(stripImageDigest(a)) == normalizeImageRef(stripImageDigest(b))
}

func imageRegistryOf(ref string) string {
	base := imageRepositoryBaseWithoutTag(imageReferenceBase(ref))
	if base == "" {
		return ""
	}
	first, _, _ := strings.Cut(base, "/")
	if !isRegistry(first) {
		return ""
	}
	return canonicalDockerHubRegistry(first)
}

func imageRepositoryParts(ref string) (name, tag string, explicit bool) {
	base := imageReferenceBase(ref)
	if base == "" {
		return "", "", false
	}
	name = base
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	if len(parts) > 0 && isRegistry(parts[0]) {
		explicit = true
		registry := canonicalDockerHubRegistry(parts[0])
		name = strings.Join(parts[1:], "/")
		if registry == "docker.io" && !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	return name, tag, explicit
}

func tagsCompatible(a, b string) bool {
	if a == "" {
		a = "latest"
	}
	if b == "" {
		b = "latest"
	}
	return a == b
}

func repositoryNameMatches(unqualified, qualified string) bool {
	if unqualified == qualified {
		return true
	}
	parts := strings.Split(unqualified, "/")
	if len(parts) == 1 {
		return qualified == unqualified || qualified == "library/"+unqualified
	}
	return false
}

// imageRepositoriesCompatible compares canonical repository keys captured
// from backend inspect responses. Unlike path compatibility it is strict:
// once a backend has supplied a registry-qualified identity, another
// registry is never interchangeable.
func imageRepositoriesCompatible(a, b string) bool {
	ak, bk := imageRepository(a), imageRepository(b)
	return ak != "" && bk != "" && strings.EqualFold(ak, bk)
}

func imageDigest(ref string) string {
	i := strings.Index(ref, "@")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}

func stripImageDigest(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// PruneReuseGroup force-removes every container tagged with the given
// WithReuseGroup value, running or stopped. Use it as a CI teardown
// step; ordinary Prune still only removes stopped managed containers.
func PruneReuseGroup(ctx context.Context, group string) ([]string, error) {
	if err := validateReuseGroupFor("PruneReuseGroup", group); err != nil {
		return nil, err
	}
	eng, err := detectEngine()
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneReuseGroupWith(ctx, defaultRunnerFor(eng), eng, group)
}

func pruneReuseGroupWith(ctx context.Context, r cli.Runner, eng engine, group string) ([]string, error) {
	if err := validateReuseGroupFor("PruneReuseGroup", group); err != nil {
		return nil, safePublicError(err, group)
	}
	return pruneListedWithGroup(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group, group)
}

func reuseContextError(ctx context.Context, name string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container: %w", name, context.DeadlineExceeded)
	}
	return ctx.Err()
}

func reuseInfoForCaller(ctx context.Context, cfg *config, base *Container) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := inspectReuseBase(ctx, base, cfg)
		if err == nil {
			if base != nil {
				base.pack().mu.Lock()
				creation := base.creation
				uid := base.immutableID()
				base.pack().mu.Unlock()
				if creation != "" && info.labels[creationLabel] != creation {
					return nil, fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
				}
				if uid != "" && info.uid != "" && info.uid != uid {
					return nil, fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
				}
			}
			if reuseInfoReady(cfg, info) {
				return info, nil
			}
			switch info.state {
			case StateStopping:
				return nil, fmt.Errorf("reuse %s: container is stopping and cannot become ready", cfg.name)
			case StatePaused:
				return nil, fmt.Errorf("reuse %s: container is paused and cannot become ready", cfg.name)
			case StateStopped:
				return nil, fmt.Errorf("reuse %s: container stopped before becoming ready", cfg.name)
			}
		} else {
			if isNotFound(err) {
				return nil, fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
			}
			if !transientReuseInspectError(err) {
				return nil, err
			}
		}
		if err := waitForReusePoll(ctx); err != nil {
			return nil, err
		}
	}
}

func reuseInfoReady(cfg *config, info *engineInfo) bool {
	return info != nil && info.state == StateRunning && reuseInfoComplete(cfg, info)
}

func reuseInfoComplete(cfg *config, info *engineInfo) bool {
	if info == nil {
		return false
	}
	if len(cfg.exposed) == 0 && len(cfg.published) == 0 {
		return true
	}
	if cfg.eng.directIP() {
		return info.ip != ""
	}
	for _, spec := range cfg.exposed {
		bound := false
		for _, b := range info.bound {
			if b.containerPort == spec.port && b.proto == spec.proto && b.hostPort > 0 {
				bound = true
				break
			}
		}
		if !bound {
			return false
		}
	}
	for _, p := range cfg.published {
		if _, ok := matchingPublishedBinding(info.bound, p, cfg.eng); !ok {
			return false
		}
	}
	return true
}
