package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reuseFlights collapses concurrent WithReuse get-or-create calls that
// share a name into one ensure operation. Each caller still applies its
// own compatibility check and wait strategy afterward.
var reuseFlights flightGroup[*Container]

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	key := cfg.eng.name() + "\x00" + cfg.name
	base, err := reuseFlights.do(ctx, key, func() (*Container, error) {
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
		defer cancel()
		return reuseEnsureContainer(flightCtx, image, cfg)
	})
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
		return nil, err
	}
	if err := attachDefaultNetworkForConfig(ctx, cfg, info); err != nil {
		return nil, err
	}
	if err := checkReuseCompat(info, image, cfg); err != nil {
		return nil, err
	}

	ctr := &Container{
		id:              base.id,
		runner:          cfg.runner,
		eng:             cfg.eng,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		defaultNetwork:  info.defaultNetwork,
		reused:          true,
		info:            immutableInfo(info),
		creation:        info.labels[creationLabel],
		uid:             info.uid,
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, err
	}

	// A readiness wait can outlive the generation returned by the
	// ensure. Re-inspect by the immutable ID and repeat compatibility checks
	// before exposing the handle.
	fresh, err := ctr.inspectDynamic(ctx)
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
	if err := checkReuseOwned(fresh, image, cfg); err != nil {
		return nil, err
	}
	if err := checkReuseCompat(fresh, image, cfg); err != nil {
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
			ctr, createErr := reuseCreate(context.WithoutCancel(ctx), image, cfg)
			if createErr == nil {
				return ctr, nil
			}
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Re-inspect and attach (or recreate) until the deadline.
			if cfg.eng.nameConflict(createErr) || createRaceMissing(createErr) {
				if err := waitForReusePoll(ctx); err != nil {
					return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
				}
				continue
			}
			return nil, createErr
		}
		lastInspectErr = nil

		if err := checkReuseOwned(info, image, cfg); err != nil {
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
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			// Only recycle containers this library created for reuse
			// with a compatible image; never delete foreign leftovers.
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return nil, err
			}
			if err := deleteStoppedReuse(ctx, cfg, info); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case StateRunning:
			return &Container{
				id:              cfg.name,
				runner:          cfg.runner,
				eng:             cfg.eng,
				exposed:         cfg.exposed,
				published:       cfg.published,
				network:         cfg.network,
				networkExplicit: cfg.networkExplicit,
				reused:          true,
				info:            immutableInfo(info),
				creation:        info.labels[creationLabel],
				uid:             info.uid,
			}, nil
		default:
			if err := waitForReusePoll(ctx); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
		}
	}
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
	if cfg.creation == "" {
		cfg.creation = newCreationID()
	}

	// The leader's pull and create get an independent runTimeout budget
	// even when the caller's context carries a tighter attach deadline.
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
	}
	preRegisterRunWithGlobalReaper(cfg)
	protectReuseReaper(cfg)
	stdout, _, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		if !attempted {
			if target, ok := runReaperTarget(cfg); ok {
				_ = unregisterWithGlobalReaper(target.binary, cfg.name, cfg.creation)
			}
			return nil, err
		}
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(err) || createRaceMissing(classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			return nil, err
		}
		if cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified); cleanupErr != nil {
			return nil, withCleanupError(classified, &CleanupError{Container: cfg.name, Err: cleanupErr})
		}
		return nil, classified
	}

	uid := cfg.eng.parseRunID(stdout)
	if requiresImmutableID(cfg.eng) && !validImmutableID(cfg.eng, uid) {
		identityErr := identityError("Docker run returned no valid immutable container ID")
		if cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr); cleanupErr != nil {
			return nil, withCleanupError(identityErr, &CleanupError{Container: cfg.name, Err: cleanupErr})
		}
		return nil, identityErr
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
		uid:             uid,
	}
	if target, ok := runReaperTarget(cfg); ok {
		if err := verifyCreatedOwnership(ctx, ctr, cfg); err != nil {
			protectReuseReaper(cfg)
			cleanupErr := cleanupFailedCreate(ctx, cfg, err, err)
			if cleanupErr != nil {
				return nil, withCleanupError(err, &CleanupError{Container: cfg.name, Err: cleanupErr})
			}
			return nil, err
		}
		_ = markSharedWithGlobalReaper(target.binary, cfg.name, cfg.creation)
		unregisterReuseFromGlobalReaper(cfg)
	}
	inspectCtx, inspectCancel := context.WithTimeout(ctx, reuseAttachTimeout)
	defer inspectCancel()
	for {
		_, err := ctr.cachedInfo(inspectCtx)
		if err == nil {
			break
		}
		if !transientReuseInspectError(err) {
			return nil, ctr.rollback(ctx, err)
		}
		if err := waitForReusePoll(inspectCtx); err != nil {
			return nil, ctr.rollback(ctx, err)
		}
	}
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return nil, ctr.rollback(ctx, err)
		}
	}
	return ctr, nil
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
		tail := ctr.logTail(context.WithoutCancel(ctx))
		if tail != "" {
			return fmt.Errorf("reuse %s failed to become ready: %w\ncontainer logs:\n%s", ctr.id, err, tail)
		}
		return fmt.Errorf("reuse %s failed to become ready: %w", ctr.id, err)
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
			ctr.uid = uid
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

// createRaceMissing reports a create/run failure that means the named
// container vanished mid-start (Apple concurrent-create race), not a
// generic "… not found" such as a missing entrypoint binary.
func createRaceMissing(err error) bool {
	command, args, ok := cliCommandParts(err)
	if !ok || command != "run" || !cliErrorBelongsTo(err, "container") {
		return false
	}
	target := cliCommandTarget(command, args)
	if target == "" {
		return false
	}
	return hasCLIErrorLine(err, func(line string) bool {
		if appleTypedContainerIDNotFoundLine(line, target) || appleIDMissingLine(line, target) {
			return true
		}
		for _, wrapper := range []string{
			"failed to bootstrap container:",
			"failed to run container:",
		} {
			rest, found := strings.CutPrefix(line, wrapper)
			if found {
				rest = strings.TrimSpace(rest)
				if appleIDMissingLine(rest, target) || appleTypedContainerIDNotFoundLine(rest, target) {
					return true
				}
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
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
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

// checkReuseOwned reports whether an existing container may be adopted
// or recycled for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if err := checkReuseIdentity(info, cfg); err != nil {
		return err
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	return nil
}

func checkReuseCompat(info *engineInfo, image string, cfg *config) error {
	if err := checkReuseOwned(info, image, cfg); err != nil {
		return err
	}
	if cfg.eng.name() == "docker" {
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
			b, ok := matchingExposedBinding(info.bound, spec)
			if !ok {
				return fmt.Errorf("reuse %s: exposed port %s missing on existing container", cfg.name, spec)
			}
			if err := checkReuseBindingReachable(cfg.eng, b); err != nil {
				return fmt.Errorf("reuse %s: exposed port %s: %w", cfg.name, spec, err)
			}
		}
	}
	for _, p := range cfg.published {
		b, ok := matchingPublishedBinding(info.bound, p)
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

func checkReuseBindingReachable(eng engine, b boundPort) error {
	if eng.name() != "docker" {
		return nil
	}
	_, err := dockerBindingConnectHost(b, eng)
	return err
}

func matchingExposedBinding(bound []boundPort, spec portSpec) (boundPort, bool) {
	var loopback boundPort
	foundLoopback := false
	for _, b := range bound {
		if b.containerPort != spec.port || b.proto != spec.proto || b.hostPort <= 0 {
			continue
		}
		if isRemoteDockerHost() && ipIsLoopback(b.hostAddr) {
			loopback, foundLoopback = b, true
			continue
		}
		return b, true
	}
	return loopback, foundLoopback
}

func matchingPublishedBinding(bound []boundPort, p publishSpec) (boundPort, bool) {
	var loopback boundPort
	foundLoopback := false
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
		if isRemoteDockerHost() && ipIsLoopback(b.hostAddr) {
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
	if requested == actual {
		return true
	}
	reqDigest := imageDigest(requested)
	actDigest := imageDigest(actual)
	if reqDigest != "" {
		if reqDigest != actDigest {
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
				base.mu.Lock()
				creation := base.creation
				uid := base.uid
				base.mu.Unlock()
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
		if _, ok := matchingPublishedBinding(info.bound, p); !ok {
			return false
		}
	}
	return true
}
