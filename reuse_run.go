package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
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

// reuseFlightKey identifies one get-or-create request. Creation-time resource
// and security options are part of the identity: callers that ask for the same
// name with different settings must not share a leader's result. The order of
// capabilities, ulimits, extra hosts, and network aliases is irrelevant.
func reuseFlightKey(cfg *config, image string) string {
	parts := []string{cfg.eng.name(), cfg.eng.targetID(), cfg.name, image, cfg.platform, cfg.network, fmt.Sprint(cfg.networkExplicit), cfg.reuseGroup, cfg.runOptionsKey(), cfg.reuseConfigHash()}
	ports := make([]string, 0, len(cfg.exposed)+len(cfg.published))
	for _, port := range cfg.exposed {
		ports = append(ports, "expose="+port.String())
	}
	for _, port := range cfg.published {
		ports = append(ports, fmt.Sprintf("publish=%s:%d:%d/%s", port.HostIP(), port.HostPort, port.ContainerPort, port.Proto))
	}
	sort.Strings(ports)
	parts = append(parts, ports...)
	for i, part := range parts {
		parts[i] = strconv.Quote(part)
	}
	data, _ := json.Marshal(parts)
	return string(data)
}

// runOptionsKey returns a canonical, order-independent representation of the
// creation-time options that distinguish a reuse request.
func (c *config) runOptionsKey() string {
	parts := make([]string, 0, 2+len(c.capAdd)+len(c.capDrop)+len(c.ulimits)+len(c.extraHosts))
	if c.cpus > 0 {
		parts = append(parts, "cpus="+formatCPUs(c.cpus))
	}
	if !c.shmSize.IsZero() {
		// Hash the normalized byte count so equivalent spellings such as
		// "64M" and "67108864" share one flight.
		parts = append(parts, "shm-size="+strconv.FormatUint(c.shmSize.Bytes(), 10))
	}
	if c.privileged {
		parts = append(parts, "privileged")
	}
	for _, capability := range c.capAdd {
		parts = append(parts, "cap-add="+capabilityKey(capability))
	}
	for _, capability := range c.capDrop {
		parts = append(parts, "cap-drop="+capabilityKey(capability))
	}
	for _, limit := range c.ulimits {
		parts = append(parts, "ulimit="+limit.arg())
	}
	for _, host := range c.extraHosts {
		parts = append(parts, "add-host="+host.arg())
	}
	if c.hostname != "" {
		parts = append(parts, "hostname="+c.hostname)
	}
	for _, alias := range c.networkAliases {
		parts = append(parts, "network-alias="+alias)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
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
		pullCtx, cancel := withDefaultTimeout(ctx, cfg.effectiveRunTimeout())
		defer cancel()
		if err := cfg.pullImage(pullCtx, image, cfg.platform); err != nil {
			return nil, err
		}
		cfg.imagePrepared = true
	}
	key := reuseFlightKey(cfg, image) + cfg.operationBudgetKey() + fmt.Sprintf("\x00attach=%d", cfg.effectiveReuseAttachTimeout())
	var leaderWarning error
	base, leader, err := reuseFlights.doWithLeader(ctx, key, func() (*Container, error) {
		// Do not detach a canceled preflight into a new shared flight.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.effectiveReuseAttachTimeout())
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

	// Mutable image tags may move during the shared ensure.
	resolvedImage, err := cfg.ensureImageRef(ctx, image)
	if err != nil {
		return nil, err
	}

	// The ensure result may have been inspected before a peer changed
	// network or port state. Once Docker has supplied an immutable ID,
	// every compatibility inspect must target that ID rather than the
	// reusable name.
	infoCtx, infoCancel := context.WithTimeout(ctx, cfg.effectiveReuseAttachTimeout())
	defer infoCancel()
	info, err := reuseInfoForCaller(infoCtx, cfg, base, resolvedImage, image)
	if err != nil {
		if errors.Is(err, ErrReuseMismatch) {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, joinEnvFileCleanupError(err, leaderWarning)
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("reuse %s: timed out waiting for complete inspect: %w", cfg.name, err)
		}
		var invalid *reuseValidationError
		if errors.As(err, &invalid) {
			return nil, joinEnvFileCleanupError(err, leaderWarning)
		}
		if leader && leaderWarning != nil {
			// The create itself succeeded and the only known warning is
			// cleanup. Preserve that usable handle even if the fresh
			// verification needed for compatibility cannot complete.
			return base, joinEnvFileCleanupError(err, leaderWarning)
		}
		return nil, err
	}

	if cfg.eng.capabilities().strictImageIdentity && resolvedImage.pinned {
		// A pinned reuse run requires an existing container to be
		// running the requested root digest. Docker enforces this via
		// inspect; Apple uses the same post-bootstrap probe on fresh
		// reuse adoption that verifyStrictCreatedImage closes for create.
		// The caller's compatibility inspect already described this
		// generation, so reuse it instead of spawning another inspect.
		probe := namedContainer(cfg, cfg.name)
		probe.image = resolvedImage
		if err := verifyStrictCreatedImage(ctx, probe, resolvedImage, info); err != nil {
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
		queryTimeout:    cfg.queryTimeout,
		probeTimeout:    cfg.probeTimeout,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		reused:          true,
		created:         cfg.reusedCreated,
		creation:        info.labels[creationLabel],
		image:           containerImage,
		sync:            &lockPack{uid: info.uid, info: immutableInfo(info), defaultNetwork: info.defaultNetwork},
		dockerMeta:      cfg.dockerMeta,
	}
	ctr.applyConfigDiagnostics(cfg)
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
	startTestLogger(cfg, ctr)
	if err := runPostReadyHooks(ctx, ctr, cfg.postReadyHooks); err != nil {
		if cfg.reusedCreated {
			// The caller that created this generation owns its initialization.
			// Discard the generation so a later Run does not attach to a
			// container whose creator hook failed; peers detect the
			// replacement through ErrGenerationReplaced.
			err = cleanupReuseCreate(ctx, ctr, err)
		}
		return nil, joinEnvFileCleanupError(err, leaderWarning)
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
	resolveImage := func(resolveCtx context.Context) error {
		if imageResolved {
			return nil
		}
		var err error
		for {
			resolvedImage, err = cfg.ensureImageRef(resolveCtx, image)
			if err == nil {
				break
			}
			if retryErr := inspectRetryError(resolveCtx, err); retryErr != nil {
				return retryErr
			}
			if !isImageQueryDeadline(err) {
				return err
			}
			if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
				return err
			}
		}
		imageResolved = true
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil && !isNotFoundFor(cfg.eng, err) {
			if ctx.Err() != nil {
				return nil, reuseInspectWaitError(cfg.name, ctx.Err(), lastInspectErr)
			}
			if errors.Is(err, context.Canceled) {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			if err := inspectRetryError(ctx, err); err != nil {
				return nil, err
			}
			lastInspectErr = err
			if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			continue
		}
		lastInspectErr = nil
		exists := err == nil
		var state domain.State
		var ownedErr, imageErr error
		if exists {
			state = info.state
			// Resolve the image and verify its presence and digest identity
			// before the decision so the pure function only sees the result.
			if err := resolveImage(context.WithoutCancel(ctx)); err != nil {
				return nil, err
			}
			ownedErr = checkReuseOwnership(info, cfg)
			imageErr = ownedErr
			if ownedErr == nil {
				imageErr = checkReuseImageIdentity(info, resolvedImage, image, cfg)
			}
		}
		decision := domain.DecideReuse(domain.ReuseInput{
			Exists:            exists,
			State:             state,
			OwnershipVerified: ownedErr == nil,
			ImageCompatible:   imageErr == nil,
			Recreated:         recreated,
		})
		switch decision.Action {
		case domain.ReuseCreateNew:
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			// Resolve the caller's image immediately before create: a
			// tag might have changed while waiting, and we want the
			// immutable image that will actually be passed to run.
			if err := resolveImage(context.WithoutCancel(ctx)); err != nil {
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
				if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
					return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
				}
				continue
			}
			return nil, createErr
		case domain.ReuseStartExisting:
			if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			continue
		case domain.ReuseRecreateIncompatible:
			// Only recycle containers this library created for reuse with a
			// compatible resolved image; never delete foreign leftovers. The
			// pure decision selects this only after ownership and image
			// identity matched above.
			if err := deleteStoppedReuse(ctx, cfg, info); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case domain.ReuseAttachRunning:
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
				creation:        info.labels[creationLabel],
				image:           resolvedImage,
				sync:            &lockPack{uid: info.uid, info: immutableInfo(info)},
				dockerMeta:      cfg.dockerMeta,
			}
			ctr.applyConfigDiagnostics(cfg)
			return ctr, nil
		case domain.ReuseFailMismatch:
			if ownedErr != nil {
				return nil, ownedErr
			}
			if imageErr != nil {
				return nil, imageErr
			}
			if exists && info.state == StateStopped && recreated {
				return nil, fmt.Errorf("reuse: %s", decision.Reason)
			}
			return nil, fmt.Errorf("reuse %s: %s", cfg.name, decision.Reason)
		default:
			return nil, fmt.Errorf("reuse %s: unexpected decision %q", cfg.name, decision.Action)
		}
	}
}

func reuseCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), cfg.effectiveRunTimeout())
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
		if cleanupErr := ctr.Terminate(context.WithoutCancel(ctx)); cleanupErr != nil {
			return nil, errors.Join(contextErr, fmt.Errorf("cleanup reused failed create: %w", cleanupErr))
		}
		return nil, contextErr
	}
	return ctr, nil
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

func reuseContextError(ctx context.Context, name string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container: %w", name, context.DeadlineExceeded)
	}
	return ctx.Err()
}

func reuseInfoForCaller(ctx context.Context, cfg *config, base *Container, requested imageIdentity, original string) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var pendingPort error
	for ctx.Err() == nil {
		info, err := inspectReuseBase(ctx, base, cfg)
		if err == nil {
			pendingPort = nil
			generationVerified := reuseGenerationMatches(base, info)
			var ownershipErr, imageErr, configErr error
			var waitForAddress bool
			if generationVerified && info.state == StateRunning {
				ownershipErr = checkReuseOwnership(info, cfg)
				imageErr = ownershipErr
				if ownershipErr == nil {
					imageErr = checkReuseImageIdentity(info, requested, original, cfg)
				}
				if ownershipErr == nil && imageErr == nil {
					if configNeedsDefaultNetwork(cfg) {
						name, err := base.ensureDefaultNetworkForConfig(ctx, cfg)
						if err != nil {
							return nil, err
						}
						info.defaultNetwork = name
					}
					configErr = checkReuseConfigCompat(info, cfg)
					if errors.Is(configErr, errReusePortPending) {
						pendingPort = configErr
					}
					waitForAddress = configErr == nil && cfg.eng.directIP() &&
						(len(cfg.exposed) > 0 || len(cfg.published) > 0) && info.ip == ""
				}
			}
			decision := domain.DecideReuseCompat(domain.ReuseCompatInput{
				State:              info.state,
				GenerationVerified: generationVerified,
				OwnershipVerified:  ownershipErr == nil,
				ImageCompatible:    imageErr == nil,
				ConfigCompatible:   configErr == nil,
				PortPending:        errors.Is(configErr, errReusePortPending),
				WaitForAddress:     waitForAddress,
			})
			switch decision.Action {
			case domain.ReuseAttachRunning:
				return info, nil
			case domain.ReuseStartExisting:
				// Poll below: the generation is still starting, a requested
				// port is not published yet, or its address is not reported.
			case domain.ReuseFailMismatch:
				switch {
				case !generationVerified:
					return nil, &reuseValidationError{fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)}
				case ownershipErr != nil:
					return nil, &reuseValidationError{ownershipErr}
				case imageErr != nil:
					return nil, &reuseValidationError{imageErr}
				case configErr != nil && !errors.Is(configErr, errReusePortPending):
					return nil, &reuseValidationError{configErr}
				default:
					return nil, &reuseValidationError{fmt.Errorf("reuse %s: %s", cfg.name, decision.Reason)}
				}
			default:
				return nil, &reuseValidationError{fmt.Errorf("reuse %s: unexpected decision %q", cfg.name, decision.Action)}
			}
		} else {
			if ctx.Err() != nil {
				break
			}
			if isNotFound(err) {
				return nil, &reuseValidationError{fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)}
			}
			if err := inspectRetryError(ctx, err); err != nil {
				if ctx.Err() != nil {
					break
				}
				return nil, err
			}
		}
		if waitForReusePoll(ctx, cfg.effectiveReusePollInterval()) != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && pendingPort != nil {
		return nil, fmt.Errorf("%w: %w", pendingPort, ctx.Err())
	}
	return nil, ctx.Err()
}

// reuseGenerationMatches reports whether the inspected container still
// addresses the base generation the caller joined. The base identity is
// snapshotted under its lock; the comparison itself has no side effects.
func reuseGenerationMatches(base *Container, info *engineInfo) bool {
	if base == nil {
		return true
	}
	base.pack().mu.Lock()
	creation := base.creation
	uid := base.immutableID()
	base.pack().mu.Unlock()
	if creation != "" && info.labels[creationLabel] != creation {
		return false
	}
	return uid == "" || info.uid == "" || info.uid == uid
}
