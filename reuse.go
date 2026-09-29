package container

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reuseFlights collapses concurrent WithReuse get-or-create calls that
// share a name into one ensure operation. Each caller still applies its
// own compatibility check and wait strategy afterward.
var reuseFlights flightGroup[*Container]

func lockNameForBackend(ctx context.Context, eng engine, name string) (func(), error) {
	if eng == nil || eng.name() != "apple" {
		return func() {}, nil
	}
	return lockName(ctx, name)
}

func waitReusePoll(ctx context.Context) error {
	timer := time.NewTimer(reusePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func cancelReuseReaperHandoffWithGeneration(runner cli.Runner, eng engine, name, creation, uid, logicalName string) error {
	er, ok := runner.(cli.ExternalRunner)
	if !ok || !er.External() {
		return nil
	}
	binary := er.ExternalBinary()
	if binary == "" {
		binary = eng.binary()
	}
	if err := unregisterHandoffWithGlobalReaper(binary, eng.reaperSubcommand(), name, creation, uid); err != nil {
		return fmt.Errorf("reuse %s: cancel reaper ownership: %w", logicalName, err)
	}
	return nil
}

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	// PullAlways is a per-caller side effect. Do it before joining the
	// shared ensure flight so an attaching caller cannot silently inherit
	// another caller's pull decision.
	if cfg.pullPolicy == PullAlways {
		prepared, err := cfg.ensureImageRef(ctx, image)
		if err != nil {
			return nil, err
		}
		cfg.preparedImage = prepared
		cfg.imagePrepared = true
	}

	key := cfg.eng.name() + "\x00" + cfg.name
	base, err := reuseFlights.do(ctx, key, func() (*Container, error) {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
		defer cancel()
		return reuseEnsureContainer(flightCtx, image, cfg)
	})
	if err != nil {
		if base != nil && keepContainers() {
			return base, err
		}
		return nil, err
	}

	info := base.info
	if info == nil {
		info, err = inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			return nil, err
		}
	}
	// A shared ensure may have run with another caller's image or pull
	// policy. Resolve this caller's request independently before comparing
	// it with the live generation.
	resolvedImage := cfg.preparedImage
	if !cfg.imagePrepared {
		resolvedImage, err = cfg.ensureImageRef(ctx, image)
		if err != nil {
			return nil, err
		}
	}
	info, err = resolveInfoPlatform(ctx, cfg.eng, cfg.runner, info, cfg.platform)
	if err != nil {
		return nil, fmt.Errorf("reuse %s: resolve existing platform: %w", cfg.name, err)
	}
	if err := checkReuseCompatIdentity(info, resolvedImage, image, cfg); err != nil {
		return nil, err
	}

	containerImage := imageFromInfo(info)
	if resolvedImage.pinned {
		containerImage = resolvedImage
	}
	ctr := &Container{
		id:                base.id,
		runner:            base.runner,
		eng:               base.eng,
		exposed:           cfg.exposed,
		published:         cfg.published,
		reused:            true,
		info:              info,
		creation:          info.labels[creationLabel],
		uid:               info.uid,
		requestedPlatform: cfg.platform,
		imageIdentity:     containerImage,
		image:             containerImage,
	}
	// Files are caller-specific side effects. Apply them after the shared
	// ensure, while holding the backend's safe identity guard, and never
	// roll back a shared generation when the copy fails.
	if err := copyReuseFiles(ctx, ctr, cfg.files); err != nil {
		return reuseHandleOnError(ctx, cfg, ctr, image, err)
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return reuseHandleOnError(ctx, cfg, ctr, image, err)
	}

	// Readiness and copying can outlive the first inspect. Revalidate the
	// exact generation under the Apple name lock before publishing a
	// handle; Docker is bound to its immutable UID and is re-inspected too.
	unlock, err := lockReuseFinal(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
	}
	defer unlock()
	fresh, err := ctr.inspectFreshLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
	}
	if err := verifyReuseResult(info, fresh, image, cfg); err != nil {
		return nil, err
	}
	if err := verifyContainerImageIdentity(ctr, fresh); err != nil {
		return nil, err
	}
	ctr.mu.Lock()
	ctr.info = fresh
	ctr.mu.Unlock()
	ctr.inspectMu.Lock()
	ctr.creation = fresh.labels[creationLabel]
	ctr.bootstrap = false
	ctr.inspectMu.Unlock()
	ctr.setImmutableID(fresh.uid)

	// A reused generation is handed off to the caller, not owned by the
	// watchdog. This also removes an older normal-run entry for the same
	// logical name/UID, so a subsequent parent death cannot delete a
	// successfully attached shared container.
	if err := cancelReuseReaperHandoffWithGeneration(ctr.runner, ctr.eng, ctr.id, ctr.creation, ctr.immutableID(), cfg.name); err != nil {
		return nil, err
	}
	return ctr, nil
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller wait or port compatibility checks. Those run in
// reuseRun so every concurrent caller applies its own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false
	var resolvedImage imageIdentity
	imageResolved := false
	resolveImage := func() error {
		if imageResolved {
			return nil
		}
		if cfg.imagePrepared {
			resolvedImage = cfg.preparedImage
			imageResolved = true
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
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("reuse %s: timed out waiting for a usable container", cfg.name)
			}
			return nil, err
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if !isNotFound(err) {
				return nil, err
			}
			// Resolve before create so compatibility is based on the
			// immutable image that will actually be passed to run.
			if err := resolveImage(); err != nil {
				return nil, err
			}
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			ctr, createErr := reuseCreateResolved(context.WithoutCancel(ctx), image, cfg, resolvedImage)
			if createErr == nil {
				return ctr, nil
			}
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Re-inspect and attach (or recreate) until the deadline.
			if cfg.eng.nameConflict(createErr) || createRaceMissing(createErr) {
				if err := waitReusePoll(ctx); err != nil {
					return nil, err
				}
				continue
			}
			if ctr != nil {
				return ctr, createErr
			}
			return nil, createErr
		}

		switch info.state {
		case StateCreated, StateStopping, StateUnknown:
			if err := waitReusePoll(ctx); err != nil {
				return nil, err
			}
			continue
		case StateStopped:
			if recreated {
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			if err := resolveImage(); err != nil {
				return nil, err
			}
			// Only recycle containers this library created for reuse
			// with a compatible resolved image; never delete foreign
			// leftovers or a container whose image cannot be verified.
			if err := checkReuseOwnedIdentity(info, resolvedImage, image, cfg); err != nil {
				return nil, err
			}
			if err := deleteStoppedReuse(ctx, cfg, info); err != nil {
				return nil, err
			}
			cfg.clearReuseCreated()
			recreated = true
			continue
		case StateRunning:
			if err := resolveImage(); err != nil {
				return nil, err
			}
			if err := checkReuseOwnedIdentity(info, resolvedImage, image, cfg); err != nil {
				return nil, err
			}
			return &Container{
				id:            cfg.name,
				runner:        cfg.runner,
				eng:           cfg.eng,
				exposed:       cfg.exposed,
				published:     cfg.published,
				reused:        true,
				info:          info,
				creation:      info.labels[creationLabel],
				uid:           info.uid,
				imageIdentity: resolvedImage,
				image:         resolvedImage,
			}, nil
		default:
			if err := waitReusePoll(ctx); err != nil {
				return nil, err
			}
		}
	}
}

func runCreateLocked(ctx context.Context, cfg *config, args ...string) (stdout []byte, attempted bool, err error) {
	if cfg.eng.name() != "apple" {
		stdout, _, err = cfg.runner.Run(ctx, args...)
		return stdout, true, err
	}
	lockCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	unlock, err := lockName(lockCtx, cfg.name)
	if err != nil {
		return nil, false, fmt.Errorf("create %s: lock name: %w", cfg.name, err)
	}
	defer unlock()
	stdout, _, err = cfg.runner.Run(ctx, args...)
	return stdout, true, err
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

func reuseCreateResolved(ctx context.Context, image string, cfg *config, resolvedImage imageIdentity) (*Container, error) {
	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}
	// Every actual backend create gets a new generation. A retry after
	// a name conflict, ambiguous failure, or stopped-container deletion
	// must not reuse the generation carried by an old handle.
	cfg.creation = newCreationID()
	// Keep the resolved image available to failed-create verification.
	// In particular, KEEP must compare the retained handle with the image
	// requested for this attempt rather than validating info against itself.
	cfg.preparedImage = resolvedImage
	cfg.imagePrepared = true

	// The leader's create gets an independent runTimeout budget even
	// when the caller's context carries a tighter attach deadline.
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	stdout, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, resolvedImage.reference, envFile)...)
	if err != nil && !attempted {
		return nil, err
	}
	if err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(err) || createRaceMissing(classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			return nil, err
		}
		if keepContainers() {
			retained, retainedErr := retainedFailedCreate(ctx, cfg, err, classified)
			return retained, withCleanupError(classified, retainedErr)
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, withCleanupError(classified, leftoverContainer(cfg.name, cleanupErr))
	}

	uid := cfg.eng.parseRunID(stdout)
	ctr := &Container{
		id:                cfg.name,
		runner:            cfg.runner,
		eng:               cfg.eng,
		exposed:           cfg.exposed,
		published:         cfg.published,
		reused:            true,
		creation:          cfg.creation,
		uid:               uid,
		requestedPlatform: cfg.platform,
		imageIdentity:     resolvedImage,
		image:             resolvedImage,
	}
	info, err := ctr.cachedInfo(ctx)
	if err != nil {
		return reuseValidationFailure(ctx, cfg, ctr, resolvedImage, err)
	}
	if err := checkReuseCompatIdentity(info, resolvedImage, image, cfg); err != nil {
		return reuseValidationFailure(ctx, cfg, ctr, resolvedImage, err)
	}
	if err := verifyContainerImageIdentity(ctr, info); err != nil {
		return reuseValidationFailure(ctx, cfg, ctr, resolvedImage, err)
	}
	cfg.markReuseCreated(ctr)
	if err := cancelReuseReaperHandoffWithGeneration(ctr.runner, ctr.eng, ctr.id, ctr.creation, ctr.immutableID(), cfg.name); err != nil {
		return nil, err
	}
	return ctr, nil
}

func verifyReuseCleanupImage(ctx context.Context, cfg *config, ctr *Container, expected imageIdentity) error {
	if ctr == nil {
		return errIdentity("nil reusable container handle")
	}
	verifyCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	info, err := ctr.inspectFresh(verifyCtx)
	if err != nil {
		return err
	}
	return checkReuseOwnedIdentity(info, expected, expected.reference, cfg)
}

func reuseHandleOnError(ctx context.Context, cfg *config, ctr *Container, original string, cause error) (*Container, error) {
	if !keepContainers() || ctr == nil {
		return nil, cause
	}
	verifyCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	info, err := ctr.inspectFresh(verifyCtx)
	if err != nil {
		return nil, withCleanupError(cause, err)
	}
	if err := checkReuseOwnedIdentity(info, ctr.imageIdentity, original, cfg); err != nil {
		return nil, withCleanupError(cause, err)
	}
	if err := verifyContainerImageIdentity(ctr, info); err != nil {
		return nil, withCleanupError(cause, err)
	}
	if err := cancelReuseReaperHandoffWithGeneration(ctr.runner, ctr.eng, ctr.id, ctr.creation, ctr.immutableID(), ctr.id); err != nil {
		return nil, withCleanupError(cause, leftoverContainer(ctr.id, err))
	}
	ctr.mu.Lock()
	ctr.info = info
	ctr.mu.Unlock()
	ctr.inspectMu.Lock()
	ctr.creation = info.labels[creationLabel]
	ctr.bootstrap = false
	ctr.inspectMu.Unlock()
	ctr.setImmutableID(info.uid)
	return ctr, cause
}

func reuseValidationFailure(ctx context.Context, cfg *config, ctr *Container, expected imageIdentity, cause error) (*Container, error) {
	if keepContainers() {
		partial, verifyErr := retainedFailedCreate(ctx, cfg, cause, cause)
		if partial != nil && verifyErr == nil {
			if err := cancelReuseReaperHandoffWithGeneration(partial.runner, partial.eng, partial.id, partial.creation, partial.immutableID(), partial.id); err != nil {
				return nil, withCleanupError(cause, leftoverContainer(partial.id, err))
			}
			return partial, cause
		}
		return nil, withCleanupError(cause, verifyErr)
	}
	if err := verifyReuseCleanupImage(ctx, cfg, ctr, expected); err != nil {
		return nil, withCleanupError(cause, leftoverContainer(cfg.name, err))
	}
	cleanupErr := cleanupFailedCreate(ctx, cfg, cause, cause)
	return nil, withCleanupError(cause, leftoverContainer(cfg.name, cleanupErr))
}

func copyReuseFiles(ctx context.Context, ctr *Container, files []File) error {
	if len(files) == 0 {
		return nil
	}
	for _, f := range files {
		if err := ctr.copyReuseFile(ctx, f); err != nil {
			return fmt.Errorf("reuse %s: WithFiles copy %q to %q: %w", ctr.id, f.HostPath, f.ContainerPath, err)
		}
	}
	return nil
}

func (c *Container) copyReuseFile(ctx context.Context, f File) error {
	target, unlock, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := c.inspectFreshLocked(ctx)
	if err != nil {
		return err
	}
	if info.state != StateRunning {
		return fmt.Errorf("container state is %s before WithFiles copy", info.state)
	}
	if err := verifyContainerImageIdentity(c, info); err != nil {
		return err
	}
	if _, _, err := c.runner.Run(ctx, c.eng.copyToArgs(target, f.HostPath, f.ContainerPath)...); err != nil {
		return c.classify(ctx, err)
	}
	after, err := c.inspectFreshLocked(ctx)
	if err != nil {
		return err
	}
	if after.state != StateRunning || !sameEngineIdentity(c.eng, info, after) {
		return generationReplaced(c.id)
	}
	if err := verifyContainerImageIdentity(c, after); err != nil {
		return err
	}
	return nil
}

// deleteStoppedReuse removes a stopped reuse container only after a
// fresh, ownership- and image-checked observation under the stable name
// lock. A changed generation is left for the next ensure iteration; a
// same-generation running container is never force-deleted.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	if keepContainers() {
		return fmt.Errorf("reuse %s: stopped container retained because CONTAINERGO_KEEP=1", cfg.name)
	}
	if info == nil || info.labels[managedLabel] != "true" ||
		info.labels[reuseLabel] != "true" ||
		!validCreationGeneration(info.labels[creationLabel]) ||
		(cfg.reusedCreated && !cfg.reuseCreatedMatches(info)) {
		return generationReplaced(cfg.name)
	}
	unlock, err := lockNameForBackend(ctx, cfg.eng, cfg.name)
	if err != nil {
		return fmt.Errorf("reuse %s: lock stopped generation: %w", cfg.name, err)
	}
	defer unlock()

	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = info.labels[creationLabel]
	ctr.setImmutableID(info.uid)
	ctr.requestedPlatform = cfg.platform
	ctr.identityOptional = cfg.eng.name() == "apple"
	ctr.bootstrap = false
	fresh, err := ctr.inspectFreshLocked(ctx)
	if isNotFound(err) {
		return nil
	}
	if errors.Is(err, ErrGenerationReplaced) && cfg.eng.name() == "apple" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reuse %s: re-inspect stopped generation: %w", cfg.name, err)
	}
	if !sameEngineIdentity(cfg.eng, info, fresh) {
		return nil
	}
	if err := validateReuseIdentity(cfg.eng, fresh); err != nil {
		return generationReplaced(cfg.name)
	}
	if fresh.imageID != info.imageID || fresh.imageDigest != info.imageDigest {
		return fmt.Errorf("%w: reuse %s: stopped generation image identity changed", ErrImageIdentityMismatch, cfg.name)
	}
	if dockerCreation(fresh) != dockerCreation(info) {
		return fmt.Errorf("reuse %s: stopped generation creation metadata changed", cfg.name)
	}
	if fresh.state != StateStopped {
		return fmt.Errorf("reuse %s: generation changed to %s; refusing to delete", cfg.name, fresh.state)
	}
	if fresh.labels[managedLabel] != "true" || fresh.labels[reuseLabel] != "true" ||
		!validCreationGeneration(fresh.labels[creationLabel]) {
		return generationReplaced(cfg.name)
	}
	if cfg.platform != "" {
		if err := checkPlatformCompatibility(cfg.platform, fresh.platform); err != nil {
			return fmt.Errorf("reuse %s: stopped generation platform changed: %w", cfg.name, err)
		}
	}
	if fresh.image != info.image {
		return fmt.Errorf("%w: reuse %s: stopped generation image changed", ErrImageIdentityMismatch, cfg.name)
	}
	if cfg.eng.name() == "docker" {
		if !validDockerUID(fresh.uid) {
			return generationReplaced(cfg.name)
		}
		return ctr.delete(ctx, fresh.uid)
	}
	if fresh.uid != "" {
		return generationReplaced(cfg.name)
	}
	return ctr.delete(ctx, cfg.name)
}

func lockReuseFinal(ctx context.Context, cfg *config) (func(), error) {
	if cfg.eng.name() != "apple" {
		return func() {}, nil
	}
	return lockNameForBackend(ctx, cfg.eng, cfg.name)
}

func verifyReuseResult(before, fresh *engineInfo, original string, cfg *config) error {
	if !sameEngineIdentity(cfg.eng, before, fresh) {
		return fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
	}
	if before.image != fresh.image {
		return fmt.Errorf("%w: reuse %s: image changed from %q to %q before return", ErrImageIdentityMismatch, cfg.name, before.image, fresh.image)
	}
	if before.imageID != fresh.imageID || before.imageDigest != fresh.imageDigest {
		return fmt.Errorf("%w: reuse %s: image identity changed before return", ErrImageIdentityMismatch, cfg.name)
	}
	if dockerCreation(before) != dockerCreation(fresh) {
		return fmt.Errorf("reuse %s: creation metadata changed before return", cfg.name)
	}
	if before.state != fresh.state || fresh.state != StateRunning {
		return fmt.Errorf("reuse %s: state changed to %s before return", cfg.name, fresh.state)
	}
	if !platformObservationUnchanged(before.platform, fresh.platform) {
		return fmt.Errorf("reuse %s: platform changed from %q to %q before return", cfg.name, before.platform, fresh.platform)
	}
	if cfg.platform != "" {
		if err := checkPlatformCompatibility(cfg.platform, fresh.platform); err != nil {
			return fmt.Errorf("reuse %s: %w", cfg.name, err)
		}
	}
	if !sameReusePorts(before.bound, fresh.bound) {
		return fmt.Errorf("reuse %s: published ports changed before return", cfg.name)
	}
	return checkReuseCompatIdentity(fresh, imageFromInfo(fresh), original, cfg)
}

func sameReusePorts(before, fresh []boundPort) bool {
	if len(before) != len(fresh) {
		return false
	}
	counts := make(map[boundPort]int, len(before))
	for _, p := range before {
		counts[p]++
	}
	for _, p := range fresh {
		if counts[p] == 0 {
			return false
		}
		counts[p]--
	}
	return true
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
	if cfg.waitStrategy == nil {
		return nil
	}
	if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: ctr}); err != nil {
		waitErr := fmt.Errorf("reuse %s failed to become ready: %w", ctr.id, err)
		if errors.Is(err, ErrGenerationReplaced) || isNotFound(err) {
			return errors.Join(waitErr, generationReplaced(ctr.id))
		}
		unlock, verifyErr := verifyReuseWaitIdentity(context.WithoutCancel(ctx), ctr)
		if verifyErr != nil {
			return errors.Join(waitErr, fmt.Errorf("reuse %s: verify after wait: %w", cfg.name, verifyErr))
		}
		defer unlock()
		target := ctr.id
		if ctr.eng.name() == "docker" {
			target = ctr.operationTarget()
			if !validDockerUID(target) {
				return errors.Join(waitErr, generationReplaced(ctr.id))
			}
		}
		tail := ctr.logTailTarget(context.WithoutCancel(ctx), target)
		if tail != "" {
			return fmt.Errorf("%w\ncontainer logs:\n%s", waitErr, tail)
		}
		return waitErr
	}
	return nil
}

func verifyReuseWaitIdentity(ctx context.Context, ctr *Container) (func(), error) {
	noop := func() {}
	if ctr.eng.name() == "apple" {
		unlock, err := lockName(ctx, ctr.id)
		if err != nil {
			return noop, fmt.Errorf("lock name: %w", err)
		}
		info, err := ctr.inspectFreshLocked(ctx)
		if err != nil {
			unlock()
			return noop, err
		}
		if err := checkFreshReusePlatform(ctr, info); err != nil {
			unlock()
			return noop, err
		}
		return unlock, nil
	}
	if ctr.eng.name() != "docker" {
		return noop, errIdentity("unknown backend cannot verify reuse identity")
	}
	info, err := ctr.inspectFresh(ctx)
	if err != nil {
		return noop, err
	}
	if err := checkFreshReusePlatform(ctr, info); err != nil {
		return noop, err
	}
	return noop, nil
}

func checkFreshReusePlatform(ctr *Container, info *engineInfo) error {
	if ctr.requestedPlatform == "" {
		return nil
	}
	if err := checkPlatformCompatibility(ctr.requestedPlatform, info.platform); err != nil {
		return err
	}
	return nil
}

func inspectNamed(ctx context.Context, cfg *config, id string) (*engineInfo, error) {
	return namedContainer(cfg, id).inspectFresh(ctx)
}

func (c *config) markReuseCreated(ctr *Container) {
	if c == nil || ctr == nil {
		return
	}
	c.reusedCreated = true
	c.reusedCreatedUID = ctr.immutableID()
	c.reusedCreatedGen = ctr.creation
}

func (c *config) reuseCreatedMatches(info *engineInfo) bool {
	if c == nil || !c.reusedCreated || info == nil {
		return false
	}
	if c.reusedCreatedUID != "" || info.uid != "" {
		return c.reusedCreatedUID != "" && c.reusedCreatedUID == info.uid
	}
	return c.reusedCreatedGen != "" && c.reusedCreatedGen == info.labels[creationLabel]
}

func (c *config) clearReuseCreated() {
	if c == nil {
		return
	}
	c.reusedCreated = false
	c.reusedCreatedUID = ""
	c.reusedCreatedGen = ""
}

func namedContainer(cfg *config, id string) *Container {
	return &Container{
		id:                id,
		runner:            cfg.runner,
		eng:               cfg.eng,
		exposed:           cfg.exposed,
		published:         cfg.published,
		requestedPlatform: cfg.platform,
		identityOptional:  true,
	}
}

// createRaceMissing reports a create/run failure that means the named
// container vanished mid-start (Apple concurrent-create race), not a
// generic "… not found" such as a missing entrypoint binary.
func createRaceMissing(err error) bool {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	s := strings.ToLower(cliErr.Stderr)
	if strings.Contains(s, "container with id") && strings.Contains(s, "not found") {
		return true
	}
	return strings.Contains(s, "container not found")
}

// imageFromInfo retains the identity reported while inspecting a
// container, including the digest form when the backend exposes one.
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

// checkReuseOwnedIdentity reports whether a stopped container may be
// deleted and recreated for this reuse request. The requested image is
// the identity resolved immediately before the create/attach decision,
// not the caller's mutable tag.
func checkReuseOwnedIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: existing container inspect returned no identity", cfg.name)
	}
	if info.labels[managedLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container is missing the managed label", cfg.name)
	}
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if err := validateReuseIdentity(cfg.eng, info); err != nil {
		return fmt.Errorf("reuse %s: %w: %v", cfg.name, ErrGenerationReplaced, err)
	}
	if cfg.platform != "" {
		if err := checkPlatformCompatibility(cfg.platform, info.platform); err != nil {
			return fmt.Errorf("reuse %s: %w", cfg.name, err)
		}
	}
	if requested.pinned {
		actual := imageFromInfo(info)
		if !requestedImageIdentitiesCompatible(requested, actual) {
			return fmt.Errorf("%w: reuse %s: image %q does not match existing %q", ErrImageIdentityMismatch, cfg.name, original, info.image)
		}
		return nil
	}
	compatible := imagesCompatible(original, info.image)
	if cfg.eng.name() == "apple" {
		compatible = appleImageReferencesCompatible(original, info.image)
	}
	if !compatible {
		return fmt.Errorf("%w: reuse %s: image %q does not match existing %q", ErrImageIdentityMismatch, cfg.name, original, info.image)
	}
	return nil
}

// checkReuseCompatIdentity applies the resolved image and port checks
// for a reuse request.
func checkReuseCompatIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if err := checkReuseOwnedIdentity(info, requested, original, cfg); err != nil {
		return err
	}
	// Auto-published exposed ports only appear as host bindings on
	// published-port backends. Explicit WithPublishedPort always needs
	// validation, including on direct-IP engines.
	if !cfg.eng.directIP() {
		for _, spec := range cfg.exposed {
			if !hasBoundPort(info.bound, spec.port, spec.proto) {
				return fmt.Errorf("reuse %s: exposed port %s missing on existing container", cfg.name, spec)
			}
		}
	}
	for _, p := range cfg.published {
		if !hasPublishedBinding(info.bound, p) {
			return fmt.Errorf("reuse %s: published port %s missing on existing container", cfg.name, p.raw)
		}
	}
	return nil
}

func hasBoundPort(bound []boundPort, port int, proto string) bool {
	for _, b := range bound {
		if b.containerPort == port && b.proto == proto && b.hostPort > 0 {
			return true
		}
	}
	return false
}

func hasPublishedBinding(bound []boundPort, p publishSpec) bool {
	for _, b := range bound {
		if b.containerPort != p.containerPort || b.proto != p.proto {
			continue
		}
		if p.hostPort != 0 && b.hostPort != p.hostPort {
			continue
		}
		if p.hostAddr != "" {
			want, wantErr := netip.ParseAddr(p.hostAddr)
			got, gotErr := netip.ParseAddr(b.hostAddr)
			if wantErr != nil || gotErr != nil || want != got {
				continue
			}
		}
		return true
	}
	return false
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
		if reqDigest != actDigest {
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
		registry = parts[0]
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
	switch strings.ToLower(first) {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return true
	default:
		return false
	}
}

func imageRepositoryBaseWithoutTag(base string) string {
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		return base[:i]
	}
	return base
}

// imageRepositoryPathsCompatible allows an unqualified caller spelling to
// match a registry-qualified backend spelling, while requiring both sides
// to name the same repository and tag.
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
	return first
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
		name = strings.Join(parts[1:], "/")
		if strings.EqualFold(parts[0], "docker.io") && !strings.Contains(name, "/") {
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

func imageRepositoriesCompatible(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(a, b)
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
	if group == "" {
		return nil, fmt.Errorf("reuse group must not be empty")
	}
	eng, err := detectEngine()
	if err != nil {
		return nil, err
	}
	return pruneReuseGroupWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng, group)
}

func pruneReuseGroupWith(ctx context.Context, r cli.Runner, eng engine, group string) ([]string, error) {
	return pruneListed(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]pruneCandidate, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group, group)
}
