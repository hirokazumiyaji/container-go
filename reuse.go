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
// own image pull, file copy, compatibility check, and wait strategy.
var reuseFlights flightGroup[*Container]

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

// attachWaitError describes an exhausted attach budget while keeping the
// context sentinel, so a caller can still tell an expired deadline from a
// cancelled call with errors.Is.
func attachWaitError(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container: %w", name, err)
	}
	return err
}

func reuseFailureResult(ctx context.Context, ctr *Container, err error) (*Container, error) {
	if !keepContainers() || ctr == nil {
		return nil, err
	}
	// Retention is useful for a copy/readiness failure only while the
	// exact current generation is still running.  Never expose a stale,
	// stopped, missing, or unverifiable handle to CONTAINERGO_KEEP callers.
	if ctr.verifiedCurrentHandle(ctx, true) {
		return ctr, err
	}
	return nil, err
}

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	// PullAlways is a per-caller side effect, even when the container is
	// already running. Do it before joining the shared ensure flight so
	// concurrent callers aggregate the pull instead of silently inheriting
	// the leader's result.
	if cfg.pullPolicy == PullAlways {
		if err := cfg.ensureImage(ctx, image); err != nil {
			return nil, err
		}
		cfg.imagePrepared = true
	}

	resolveCtx, attachCancel := context.WithTimeout(ctx, reuseAttachTimeout)
	defer attachCancel()
	stoppedRecreated := false
	for {
		if err := resolveCtx.Err(); err != nil {
			return nil, attachWaitError(cfg.name, err)
		}

		base, err := reuseEnsureFlight(resolveCtx, image, cfg)
		if err != nil {
			// A retained failed create may carry a partial handle, but
			// KEEP still requires a fresh identity check before exposing
			// it.  A failed/stopped/unverifiable generation is discarded.
			if base != nil && keepContainers() && base.verifiedRetainedHandle(resolveCtx) {
				return base, attachWaitError(cfg.name, err)
			}
			return nil, attachWaitError(cfg.name, err)
		}
		info, err := inspectNamed(resolveCtx, cfg, cfg.name)
		if err != nil {
			if errors.Is(err, ErrSystemNotRunning) {
				return nil, err
			}
			if isNotFound(err) {
				cfg.clearReuseCreated()
				continue
			}
			return nil, err
		}
		if cfg.reusedCreated && !cfg.reuseCreatedMatches(info) {
			return nil, fmt.Errorf("reuse %s: %w: creator generation changed", cfg.name, ErrGenerationReplaced)
		}
		if !cfg.reuseCreatedMatches(info) {
			cfg.clearReuseCreated()
		}

		switch info.state {
		case StateStopped:
			if stoppedRecreated {
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return nil, err
			}
			// Make the replacement image available before the stopped
			// generation is discarded, so a PullNever run that cannot
			// find it and a PullMissing run whose pull fails keep the
			// existing container instead of leaving the caller with
			// neither.
			if err := prepareReuseReplacement(ctx, image, cfg); err != nil {
				return nil, err
			}
			removed, err := deleteStoppedReuseChecked(resolveCtx, cfg, info)
			if err != nil {
				return nil, err
			}
			if !removed {
				// The generation may have been adopted or may be
				// stopping. Re-inspect it; never turn a refusal into a
				// create race.
				if err := waitReusePoll(resolveCtx); err != nil {
					return nil, attachWaitError(cfg.name, err)
				}
				continue
			}
			stoppedRecreated = true
			cfg.clearReuseCreated()
			continue
		case StateCreated, StateStopping, StateUnknown:
			if err := waitReusePoll(resolveCtx); err != nil {
				return nil, attachWaitError(cfg.name, err)
			}
			continue
		case StateRunning:
			// Continue below with a freshly inspected, running identity.
		default:
			if err := waitReusePoll(resolveCtx); err != nil {
				return nil, attachWaitError(cfg.name, err)
			}
			continue
		}

		if err := checkReuseCompat(info, image, cfg); err != nil {
			return nil, err
		}
		if usesImmutableIDs(base.eng) && !dockerIDRE.MatchString(info.uid) {
			return nil, fmt.Errorf("reuse %s: backend did not report a full immutable Docker ID", cfg.name)
		}
		ctr := &Container{
			id:        base.id,
			runner:    base.runner,
			eng:       base.eng,
			exposed:   cfg.exposed,
			published: cfg.published,
			reused:    true,
			info:      info,
			creation:  info.labels[creationLabel],
		}
		ctr.rememberImmutableID(info.uid)
		// The attach budget only bounds polling for a usable container.
		// From here the generation is known, so file application, the
		// wait strategy, and the final verification run on the caller's
		// own context: a long copy or a readiness wait must not be cut off
		// by the attach deadline that has already been satisfied.
		attachCancel()
		if err := copyReuseFiles(ctx, ctr, cfg.files); err != nil {
			return reuseFailureResult(ctx, ctr, err)
		}
		if err := reuseWait(ctx, cfg, ctr); err != nil {
			return reuseFailureResult(ctx, ctr, err)
		}

		// A wait strategy or a concurrent replacement can outlive the
		// inspect above. Never return a handle for a different, stopped,
		// or otherwise changed generation.
		unlock, err := lockReuseFinal(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
		}
		defer unlock()
		// Docker handles still use their immutable UID for operations,
		// but the final reuse check intentionally inspects the name to
		// detect a same-name replacement generation. Apple is already
		// holding the name lock here, so use a raw name inspect without
		// recursively taking that lock.
		fresh, err := ctr.inspectTargetFreshRetry(ctx, ctr.id)
		if err != nil {
			// The operation itself failed and the final identity could not
			// be proven.  KEEP must not return this handle.
			return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
		}
		if usesNameAddressedDeletes(ctr.eng) {
			if err := ctr.identityMatches(fresh); err != nil {
				return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
			}
		}
		if err := verifyReuseResult(info, fresh, image, cfg); err != nil {
			return nil, err
		}
		return ctr, nil
	}
}

func reuseEnsureFlight(ctx context.Context, image string, cfg *config) (*Container, error) {
	key := cfg.eng.name() + "\x00" + cfg.name
	return reuseFlights.do(ctx, key, func() (*Container, error) {
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
		defer cancel()
		return reuseEnsureContainer(flightCtx, image, cfg)
	})
}

func (c *config) reuseCreatedMatches(info *engineInfo) bool {
	if !c.reusedCreated || info == nil {
		return false
	}
	if c.reusedCreatedUID != "" || info.uid != "" {
		return c.reusedCreatedUID != "" && c.reusedCreatedUID == info.uid
	}
	return c.reusedCreatedGeneration != "" && c.reusedCreatedGeneration == info.labels[creationLabel]
}

func (c *config) clearReuseCreated() {
	c.reusedCreated = false
	c.reusedCreatedUID = ""
	c.reusedCreatedGeneration = ""
	// A future create attempt must receive a new generation.  Keeping the
	// old value here can make a retry label a stopped or replaced
	// generation with the previous attempt's ownership token.
	c.creation = ""
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller pull, file copy, wait, or port compatibility
// checks. Those run in reuseRun so every concurrent caller applies its
// own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, attachWaitError(cfg.name, err)
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if errors.Is(err, ErrSystemNotRunning) {
				return nil, err
			}
			if !isNotFound(err) {
				return nil, err
			}
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			ctr, createErr := reuseCreate(context.WithoutCancel(ctx), image, cfg)
			if createErr == nil {
				cfg.reusedCreated = true
				cfg.reusedCreatedUID = ctr.immutableID()
				cfg.reusedCreatedGeneration = ctr.creation
				return ctr, nil
			}
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Re-inspect and attach (or recreate) until the deadline, but
			// never turn a classified daemon outage into that retry loop.
			if errors.Is(createErr, ErrSystemNotRunning) {
				return nil, createErr
			}
			if cfg.eng.nameConflict(createErr) || createRaceMissing(createErr) {
				if err := waitReusePoll(ctx); err != nil {
					return nil, attachWaitError(cfg.name, err)
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
				return nil, attachWaitError(cfg.name, err)
			}
			continue
		case StateStopped:
			if recreated {
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			// Only recycle containers this library created for reuse
			// with a compatible image; never delete foreign leftovers.
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return nil, err
			}
			// Make the replacement image available before the stopped
			// generation is discarded, so a PullNever run that cannot
			// find it and a PullMissing run whose pull fails keep the
			// existing container instead of leaving the caller with
			// neither.
			if err := prepareReuseReplacement(ctx, image, cfg); err != nil {
				return nil, err
			}
			removed, err := deleteStoppedReuseChecked(ctx, cfg, info)
			if err != nil {
				return nil, err
			}
			if !removed {
				if err := waitReusePoll(ctx); err != nil {
					return nil, attachWaitError(cfg.name, err)
				}
				continue
			}
			cfg.clearReuseCreated()
			recreated = true
			continue
		case StateRunning:
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return nil, err
			}
			if usesImmutableIDs(cfg.eng) && !dockerIDRE.MatchString(info.uid) {
				return nil, fmt.Errorf("reuse %s: backend did not report a full immutable Docker ID", cfg.name)
			}
			ctr := &Container{
				id:        cfg.name,
				runner:    cfg.runner,
				eng:       cfg.eng,
				exposed:   cfg.exposed,
				published: cfg.published,
				reused:    true,
				info:      info,
				creation:  info.labels[creationLabel],
			}
			ctr.rememberImmutableID(info.uid)
			return ctr, nil
		default:
			if err := waitReusePoll(ctx); err != nil {
				return nil, attachWaitError(cfg.name, err)
			}
		}
	}
}

func initializeReuseHandle(ctx context.Context, cfg *config, ctr *Container) error {
	if !usesNameAddressedDeletes(cfg.eng) {
		info, err := ctr.cachedInfo(ctx)
		if err != nil {
			return err
		}
		if info.labels[managedLabel] != "true" || (cfg.reuse && info.labels[reuseLabel] != "true") ||
			!creationRE.MatchString(cfg.creation) || info.labels[creationLabel] != cfg.creation {
			return fmt.Errorf("reuse %s: %w: post-create generation could not be verified", cfg.name, ErrGenerationReplaced)
		}
		return nil
	}

	guardCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	unlock, err := lockName(guardCtx, cfg.name)
	if err != nil {
		return fmt.Errorf("reuse %s: lock post-create generation: %w", cfg.name, err)
	}
	defer unlock()
	info, err := ctr.inspectTargetFreshRetry(guardCtx, cfg.name)
	if err != nil {
		return err
	}
	if err := ctr.identityMatches(info); err != nil {
		return err
	}
	if (cfg.reuse && info.labels[reuseLabel] != "true") || !creationRE.MatchString(cfg.creation) ||
		info.labels[creationLabel] != cfg.creation {
		return fmt.Errorf("reuse %s: %w: post-create generation could not be verified", cfg.name, ErrGenerationReplaced)
	}
	ctr.info = info
	return nil
}

func reuseCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}
	// Every actual create attempt gets a fresh generation, including a
	// retry after a stale/missing generation was observed.  Reusing the
	// previous value would let failed-create ownership or cleanup match
	// the wrong attempt.
	cfg.clearReuseCreated()
	cfg.creation = newCreationID()

	// The leader's pull and create get an independent runTimeout budget
	// even when the caller's context carries a tighter attach deadline.
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	if !cfg.imagePrepared {
		if err := cfg.ensureImage(runCtx, image); err != nil {
			return nil, err
		}
	}
	// Record the name and generation with the watchdog before the create
	// runs. A reuse generation is shared state, so the pending record is
	// never promoted to an active one: the child refuses to delete a
	// pending reuse generation, and a verified failed create is handed
	// over explicitly by the cleanup paths instead.
	pending := registerPendingContainerReaper(cfg)
	stdout, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		if !attempted {
			// No create command was issued, so nothing can exist under
			// this generation.
			discardPendingContainerReaper(pending)
			return nil, err
		}
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if errors.Is(classified, ErrSystemNotRunning) {
			// A daemon outage is terminal; retrying only turns one
			// classified failure into repeated probes and create races.
			return nil, classified
		}
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(err) || createRaceMissing(classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.  Return
			// the classified chain so callers do not lose daemon errors.
			return nil, classified
		}
		if usesImmutableIDs(cfg.eng) && !dockerIDRE.MatchString(cfg.eng.parseRunID(stdout)) {
			return recoverDockerRunOutput(ctx, cfg, classified)
		}
		if keepContainers() {
			retained, retainedErr := retainedFailedCreate(ctx, cfg, err, classified)
			return retained, withCleanupError(classified, retainedErr)
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, withCleanupError(classified, cleanupErr)
	}

	runID := cfg.eng.parseRunID(stdout)
	ctr := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    cfg.reuse,
		creation:  cfg.creation,
	}
	ctr.rememberImmutableID(runID)
	if usesImmutableIDs(cfg.eng) && !dockerIDRE.MatchString(ctr.immutableID()) {
		return recoverDockerRunOutput(ctx, cfg, fmt.Errorf("reuse %s: backend did not return a full immutable Docker ID", cfg.name))
	}
	if err := initializeReuseHandle(runCtx, cfg, ctr); err != nil {
		// A reuse container is shared as soon as its name is published;
		// do not let this caller remove a generation a peer may be using.
		return reuseFailureResult(ctx, ctr, err)
	}
	// WithFiles is applied by reuseRun after the shared ensure flight, not
	// here. A caller-specific copy failure must not poison waiters.
	return ctr, nil
}

type reuseCopyIdentity struct {
	uid      string
	creation string
	target   string
}

func copyReuseFiles(ctx context.Context, ctr *Container, files []File) error {
	if len(files) == 0 {
		return nil
	}
	identity, target, unlock, err := ctr.beginReuseCopy(ctx)
	if err != nil {
		return fmt.Errorf("reuse %s: WithFiles identity check: %w", ctr.id, err)
	}
	defer unlock()
	for _, f := range files {
		if err := ctr.copyToContainerTarget(ctx, target, f.HostPath, f.ContainerPath); err != nil {
			return fmt.Errorf("reuse %s: WithFiles copy %q to %q: %w", ctr.id, f.HostPath, f.ContainerPath, err)
		}
	}
	if err := ctr.verifyReuseCopyIdentity(ctx, identity); err != nil {
		return fmt.Errorf("reuse %s: WithFiles identity changed: %w", ctr.id, err)
	}
	return nil
}

func (c *Container) beginReuseCopy(ctx context.Context) (reuseCopyIdentity, string, func(), error) {
	if usesImmutableIDs(c.eng) {
		uid := c.immutableID()
		if !dockerIDRE.MatchString(uid) {
			return reuseCopyIdentity{}, "", nil, fmt.Errorf("container %s has no valid immutable Docker ID", c.id)
		}
		info, err := c.inspectTargetFreshRetry(ctx, uid)
		if err != nil {
			return reuseCopyIdentity{}, "", nil, err
		}
		if info.uid == "" || info.uid != uid {
			return reuseCopyIdentity{}, "", nil, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		if info.state != StateRunning {
			return reuseCopyIdentity{}, "", nil, fmt.Errorf("container state is %s before copy", info.state)
		}
		return reuseCopyIdentity{uid: uid, target: uid}, uid, func() {}, nil
	}
	if !creationRE.MatchString(c.creation) {
		return reuseCopyIdentity{}, "", nil, fmt.Errorf("container has no valid creation generation")
	}
	unlock := func() {}
	if usesNameAddressedDeletes(c.eng) {
		var err error
		unlock, err = lockName(ctx, c.id)
		if err != nil {
			return reuseCopyIdentity{}, "", nil, fmt.Errorf("lock name: %w", err)
		}
	}
	info, err := c.inspectTargetFreshRetry(ctx, c.id)
	if err != nil {
		unlock()
		return reuseCopyIdentity{}, "", nil, err
	}
	if info.state != StateRunning {
		unlock()
		return reuseCopyIdentity{}, "", nil, fmt.Errorf("container state is %s before copy", info.state)
	}
	if err := c.identityMatches(info); err != nil {
		unlock()
		return reuseCopyIdentity{}, "", nil, err
	}
	return reuseCopyIdentity{creation: c.creation, target: c.id}, c.id, unlock, nil
}

func (c *Container) verifyReuseCopyIdentity(ctx context.Context, identity reuseCopyIdentity) error {
	info, err := c.inspectTargetFreshRetry(ctx, identity.target)
	if err != nil {
		return err
	}
	if identity.uid != "" {
		if info.uid == "" || info.uid != identity.uid {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		return nil
	}
	if err := c.identityMatches(info); err != nil {
		return err
	}
	if info.labels[creationLabel] != identity.creation {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	return nil
}

// prepareReuseReplacement makes the requested image, and the requested
// platform variant when one was selected, available in the backend's
// local store before a stopped generation is deleted. Deleting first and
// discovering afterwards that the replacement image cannot be fetched
// would leave a reuse caller with no container at all under PullNever,
// and would pull a large image only to fail under PullMissing.
func prepareReuseReplacement(ctx context.Context, image string, cfg *config) error {
	if cfg.imagePrepared {
		return nil
	}
	// The leader's image fetch gets an independent runTimeout budget even
	// when the caller's context carries a tighter attach deadline.
	prepCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	if err := cfg.ensureImage(prepCtx, image); err != nil {
		return err
	}
	cfg.imagePrepared = true
	return nil
}

// deleteStoppedReuse removes a stopped reuse container only after
// rechecking its state and identity.  The checked form reports whether a
// delete actually happened; callers must re-inspect instead of assuming a
// non-delete means the generation was removed.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	_, err := deleteStoppedReuseChecked(ctx, cfg, info)
	return err
}

func deleteStoppedReuseChecked(ctx context.Context, cfg *config, info *engineInfo) (bool, error) {
	if err := checkReuseLabels(info, cfg); err != nil {
		return false, err
	}
	if info.state != StateStopped {
		return false, nil
	}
	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = info.labels[creationLabel]
	// Every delete below targets a generation that was just verified
	// stopped, so it omits --force: a generation that started in the
	// meantime is reported as a failure and adopted by the next poll
	// instead of being killed.
	remove := func(ctx context.Context, target string) error {
		return ctr.deleteWithArgs(ctx, target, stoppedDeleteArgsFor(cfg.eng, target))
	}

	if usesImmutableIDs(cfg.eng) {
		if !dockerIDRE.MatchString(info.uid) {
			return false, fmt.Errorf("reuse %s: stopped generation has no full immutable ID; refusing cleanup", cfg.name)
		}
		fresh, err := ctr.inspectTargetFreshRetry(ctx, info.uid)
		if isNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
		}
		if !sameStoppedGeneration(info, fresh) || fresh.state != StateStopped ||
			checkReuseLabels(fresh, cfg) != nil {
			return false, nil
		}
		// Hand the verified generation to the watchdog before the delete so
		// a failed removal can be retried after this process exits.
		ctr.rememberImmutableID(fresh.uid)
		registerContainerReaper(cfg, ctr)
		if err := remove(ctx, fresh.uid); err != nil {
			return false, err
		}
		return true, nil
	}

	if usesNameAddressedDeletes(cfg.eng) {
		guardCtx, guardCancel := withDefaultTimeout(ctx, queryTimeout)
		defer guardCancel()
		unlock, err := lockName(guardCtx, cfg.name)
		if err != nil {
			return false, fmt.Errorf("reuse %s: lock stopped generation: %w", cfg.name, err)
		}
		defer unlock()
		fresh, err := ctr.inspectTargetFreshRetry(guardCtx, cfg.name)
		if isNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
		}
		if !sameStoppedGeneration(info, fresh) || fresh.state != StateStopped ||
			checkReuseLabels(fresh, cfg) != nil {
			return false, nil
		}
		if err := remove(guardCtx, cfg.name); err != nil {
			return false, err
		}
		return true, nil
	}

	// Compatibility for small injected engines that have neither identity
	// capability.  They retain the historical name behavior, but still
	// require a stopped, matching generation before deletion.
	fresh, err := ctr.inspectTargetFreshRetry(ctx, cfg.name)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
	}
	if !sameStoppedGeneration(info, fresh) || fresh.state != StateStopped || checkReuseLabels(fresh, cfg) != nil {
		return false, nil
	}
	return true, remove(ctx, cfg.name)
}

func sameStoppedGeneration(before, fresh *engineInfo) bool {
	if before == nil || fresh == nil || before.state != StateStopped || fresh.state != StateStopped {
		return false
	}
	if before.uid != "" || fresh.uid != "" {
		return dockerIDRE.MatchString(before.uid) && before.uid == fresh.uid
	}
	return creationRE.MatchString(before.labels[creationLabel]) &&
		before.labels[creationLabel] == fresh.labels[creationLabel]
}

func classifyReuseError(ctx context.Context, cfg *config, err error) error {
	if err == nil {
		return nil
	}
	return cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
	if cfg.waitStrategy == nil {
		return nil
	}
	if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: ctr}); err != nil {
		err = classifyReuseError(ctx, cfg, err)
		tail := ctr.logTail(context.WithoutCancel(ctx))
		if tail != "" {
			return fmt.Errorf("reuse %s failed to become ready: %w\ncontainer logs:\n%s", ctr.id, err, tail)
		}
		return fmt.Errorf("reuse %s failed to become ready: %w", ctr.id, err)
	}
	return nil
}

func verifyReuseResult(before, fresh *engineInfo, image string, cfg *config) error {
	if !sameReuseGeneration(before, fresh) {
		return fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
	}
	if fresh.state != StateRunning {
		return fmt.Errorf("reuse %s: state changed to %s before return", cfg.name, fresh.state)
	}
	if before.state != fresh.state {
		return fmt.Errorf("reuse %s: state changed from %s to %s before return", cfg.name, before.state, fresh.state)
	}
	if fresh.image != before.image {
		return fmt.Errorf("reuse %s: image changed from %q to %q before return", cfg.name, before.image, fresh.image)
	}
	if !samePlatformMetadata(before.platform, fresh.platform) {
		return fmt.Errorf("reuse %s: platform changed from %q to %q before return", cfg.name, before.platform, fresh.platform)
	}
	if before.imageDigest != fresh.imageDigest {
		return fmt.Errorf("reuse %s: image descriptor digest changed before return", cfg.name)
	}
	if !sameReusePorts(before.bound, fresh.bound) {
		return fmt.Errorf("reuse %s: published ports changed before return", cfg.name)
	}
	return checkReuseCompat(fresh, image, cfg)
}

func sameReuseGeneration(before, fresh *engineInfo) bool {
	if before == nil || fresh == nil {
		return false
	}
	if before.uid != "" || fresh.uid != "" {
		return dockerIDRE.MatchString(before.uid) && before.uid == fresh.uid
	}
	return creationRE.MatchString(before.labels[creationLabel]) &&
		before.labels[creationLabel] == fresh.labels[creationLabel]
}

func sameReusePorts(before, fresh []boundPort) bool {
	if len(before) != len(fresh) {
		return false
	}
	counts := make(map[boundPort]int, len(before))
	for _, port := range before {
		counts[port]++
	}
	for _, port := range fresh {
		if counts[port] == 0 {
			return false
		}
		counts[port]--
	}
	return true
}

func lockReuseFinal(ctx context.Context, cfg *config) (func(), error) {
	if !usesNameAddressedDeletes(cfg.eng) {
		return func() {}, nil
	}
	return lockName(ctx, cfg.name)
}

func inspectNamed(ctx context.Context, cfg *config, id string) (*engineInfo, error) {
	return namedContainer(cfg, id).inspectFresh(ctx)
}

func namedContainer(cfg *config, id string) *Container {
	return &Container{
		id:          id,
		runner:      cfg.runner,
		eng:         cfg.eng,
		exposed:     cfg.exposed,
		published:   cfg.published,
		nameInspect: true,
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

// platformSelectorMatches compares the fields selected by the caller.
// An omitted selector component is a wildcard; every component that the
// caller does specify must be present in the backend's normalized value.
func parsePlatformParts(value string) (osName, arch, variant string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", "", false
	}
	parts := strings.Split(value, "/")
	if len(parts) > 3 {
		return "", "", "", false
	}
	for _, part := range parts {
		if part == "" {
			return "", "", "", false
		}
	}
	if len(parts) > 0 {
		osName = parts[0]
	}
	if len(parts) > 1 {
		arch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return osName, arch, variant, true
}

func platformSelectorMatches(requested, actual string) bool {
	if strings.TrimSpace(requested) == "" {
		return true
	}
	wantOS, wantArch, wantVariant, wantOK := parsePlatformParts(requested)
	if !wantOK {
		return false
	}
	if wantArch == "" && wantVariant == "" {
		// An OS-only selector is intentionally a wildcard for the
		// architecture and variant. Older inspect formats may not
		// report any platform at all, so absence cannot disprove it.
		gotOS, _, _, gotOK := parsePlatformParts(actual)
		return !gotOK || gotOS == "" || strings.EqualFold(wantOS, gotOS)
	}
	gotOS, gotArch, gotVariant, gotOK := parsePlatformParts(actual)
	if !gotOK {
		return false
	}
	return strings.EqualFold(wantOS, gotOS) &&
		strings.EqualFold(wantArch, gotArch) &&
		(wantVariant == "" || strings.EqualFold(wantVariant, gotVariant))
}

func platformSelectorUnverifiable(requested, actual string) bool {
	if strings.TrimSpace(requested) == "" {
		return false
	}
	wantOS, wantArch, wantVariant, wantOK := parsePlatformParts(requested)
	if !wantOK {
		return true
	}
	if wantArch == "" && wantVariant == "" {
		return false
	}
	gotOS, gotArch, gotVariant, gotOK := parsePlatformParts(actual)
	if !gotOK {
		return true
	}
	if wantOS != "" && gotOS != "" && !strings.EqualFold(wantOS, gotOS) {
		return false
	}
	if wantArch != "" && gotArch == "" {
		return true
	}
	if wantVariant != "" && gotVariant == "" {
		return true
	}
	return false
}

func samePlatformMetadata(a, b string) bool {
	if strings.TrimSpace(a) == "" && strings.TrimSpace(b) == "" {
		return true
	}
	aOS, aArch, aVariant, aOK := parsePlatformParts(a)
	bOS, bArch, bVariant, bOK := parsePlatformParts(b)
	if !aOK || !bOK {
		return false
	}
	return strings.EqualFold(aOS, bOS) &&
		strings.EqualFold(aArch, bArch) &&
		strings.EqualFold(aVariant, bVariant)
}

// checkReuseOwned reports whether an existing container may be adopted
// or deleted for this reuse request. All three labels are required: a
// reuse marker alone can be present on a foreign container, and a
// generation is what makes a later name-based delete safe.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: existing container was not found", cfg.name)
	}
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	if usesImmutableIDs(cfg.eng) && !dockerIDRE.MatchString(info.uid) {
		return fmt.Errorf("reuse %s: existing container has no valid immutable ID", cfg.name)
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	if cfg.platform != "" {
		if platformSelectorUnverifiable(cfg.platform, info.platform) {
			return fmt.Errorf("reuse %s: platform %q could not be verified", cfg.name, cfg.platform)
		}
		if !platformMatches(cfg.eng, cfg.platform, info.platform) {
			return fmt.Errorf("reuse %s: platform %q does not match existing %q", cfg.name, cfg.platform, info.platform)
		}
	}
	return nil
}

func checkReuseLabels(info *engineInfo, cfg *config) error {
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if info.labels[managedLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container is not managed by container-go", cfg.name)
	}
	if !creationRE.MatchString(info.labels[creationLabel]) {
		return fmt.Errorf("reuse %s: existing container has no valid creation generation", cfg.name)
	}
	return nil
}

func checkReuseCompat(info *engineInfo, image string, cfg *config) error {
	if err := checkReuseOwned(info, image, cfg); err != nil {
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

// hasPublishedBinding matches one configured publish spec against the
// bindings inspect reported. An explicit host address is compared as a
// parsed netip address and must match exactly: a textual comparison would
// accept a differently spelled address, and treating an unreported
// address as a wildcard would adopt a binding published on another
// interface than the caller asked for.
func hasPublishedBinding(bound []boundPort, p publishSpec) bool {
	want, wantAddr := netip.Addr{}, false
	if p.hostAddr != "" {
		parsed, err := netip.ParseAddr(p.hostAddr)
		if err != nil {
			return false
		}
		want, wantAddr = parsed, true
	}
	for _, b := range bound {
		if b.containerPort != p.containerPort || b.proto != p.proto {
			continue
		}
		if p.hostPort != 0 && b.hostPort != p.hostPort {
			continue
		}
		if wantAddr {
			got, err := netip.ParseAddr(b.hostAddr)
			if err != nil || got != want {
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
	if requested == actual {
		return true
	}
	reqDigest := imageDigest(requested)
	actDigest := imageDigest(actual)
	if reqDigest != "" {
		if actDigest == "" || !strings.EqualFold(reqDigest, actDigest) {
			return false
		}
		return normalizeImageRef(stripImageDigest(requested)) == normalizeImageRef(stripImageDigest(actual))
	}
	req := normalizeImageRef(stripImageDigest(requested))
	act := normalizeImageRef(stripImageDigest(actual))
	return req == act
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
	return pruneListedWithGroup(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group, group)
}
