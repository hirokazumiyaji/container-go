package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
		// The flight preserves a verified partial handle when a create
		// fails under CONTAINERGO_KEEP; callers that receive this shared
		// flight result use that same handle, so do not discard it here.
		return base, err
	}

	info := base.info
	if info == nil {
		info, err = inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			return nil, err
		}
	}
	if err := checkReuseCompat(info, image, cfg); err != nil {
		return reuseCompatFailure(ctx, cfg, image, base, err)
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
		uid:       info.uid,
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		if keepContainers() {
			verified, verifyErr := verifyRetainedHandle(ctx, ctr)
			if verifyErr != nil {
				return nil, withCleanupError(err, verifyErr)
			}
			return verified, err
		}
		return nil, err
	}
	return ctr, nil
}

// reuseCompatFailure handles a per-caller compatibility failure observed
// after the shared ensure.
//
// A generation this request created is this request's to resolve: under
// CONTAINERGO_KEEP=1 it is retained and returned as a verified handle,
// otherwise it is cleaned up while a fresh inspect still proves it is
// unadopted. A generation this request merely attached to belongs to
// every peer of that name, so it is never deleted: the compatibility
// error is returned as is.
func reuseCompatFailure(ctx context.Context, cfg *config, image string, base *Container, cause error) (*Container, error) {
	if base == nil || !base.createdHere {
		return nil, cause
	}
	if keepContainers() {
		verified, verifyErr := verifyRetainedHandle(ctx, base)
		if verifyErr != nil {
			return nil, withCleanupError(cause, verifyErr)
		}
		return verified, cause
	}
	if err := verifyReuseCleanupIdentity(ctx, cfg, image, base); err != nil {
		return nil, withCleanupError(cause, err)
	}
	cleanupErr := cleanupFailedCreate(ctx, cfg, cause, cause)
	return nil, withCleanupError(cause, cleanupErr)
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller wait or port compatibility checks. Those run in
// reuseRun so every concurrent caller applies its own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("reuse %s: timed out waiting for a usable container", cfg.name)
			}
			return nil, err
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if !isNotFoundFor(cfg.eng, cfg.name, err) {
				return nil, err
			}
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			ctr, createErr := reuseCreate(context.WithoutCancel(ctx), image, cfg)
			if createErr == nil {
				return ctr, nil
			}
			if ctr != nil {
				// A retained partial handle is deliberately returned
				// with the create error so callers can inspect or
				// explicitly terminate the shared container. Check this
				// before retry classification: a verified owned create
				// must not be mistaken for a peer race.
				return ctr, createErr
			}
			if keepContainers() {
				// A failed ownership lookup is not a peer race. Do not
				// retry it: without verification, returning or attaching
				// to a same-name container would be fail-open.
				var retainedErr *CleanupError
				if errors.As(createErr, &retainedErr) {
					return nil, createErr
				}
			}
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Re-inspect and attach (or recreate) until the deadline.
			// Only the primary create failure is retryable: a cleanup
			// failure in CleanupError.CleanupErr must not be read as a
			// peer win and must not restart this ensure.
			primaryErr := primaryOperationError(createErr)
			if cfg.eng.nameConflict(primaryErr) || createRaceMissing(primaryErr) {
				time.Sleep(reusePollInterval)
				continue
			}
			return nil, createErr
		}

		switch info.state {
		case StateCreated, StateStopping, StateUnknown:
			time.Sleep(reusePollInterval)
			continue
		case StateStopped:
			if recreated {
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			// This is an explicit WithReuse lifecycle step, not a Run
			// rollback; CONTAINERGO_KEEP does not suppress it.
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
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return nil, err
			}
			return &Container{
				id:        cfg.name,
				runner:    cfg.runner,
				eng:       cfg.eng,
				exposed:   cfg.exposed,
				published: cfg.published,
				reused:    true,
				info:      info,
				creation:  info.labels[creationLabel],
				uid:       info.uid,
			}, nil
		default:
			time.Sleep(reusePollInterval)
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
	stdout, _, err := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if keepContainers() {
			// A conflict or Apple create-race can happen after this
			// process created the container. Verify ownership before
			// retrying so KEEP returns an owned partial handle, while
			// leaving an unverified peer available for attach.
			retained, retainedErr := retainedFailedCreate(ctx, cfg, err, classified)
			if retained != nil || retainedErr != nil {
				return retained, withCleanupError(classified, retainedErr)
			}
		}
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(err) || createRaceMissing(classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			return nil, err
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, withCleanupError(classified, cleanupErr)
	}

	ctr := &Container{
		id:          cfg.name,
		runner:      cfg.runner,
		eng:         cfg.eng,
		exposed:     cfg.exposed,
		published:   cfg.published,
		reused:      true,
		createdHere: true,
		creation:    cfg.creation,
		uid:         cfg.eng.parseRunID(stdout),
	}
	// The create is published the moment the backend accepts it, so a
	// peer may attach to this generation at any point from here on.
	// Post-create setup failures therefore never force-delete: they
	// clean up only while the fresh inspect still proves this
	// generation exists, is unadopted, and is owned by this request.
	if _, err := ctr.cachedInfo(ctx); err != nil {
		return reuseSetupFailure(ctx, cfg, image, ctr, err)
	}
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return reuseSetupFailure(ctx, cfg, image, ctr, err)
		}
	}
	return ctr, nil
}

// reuseSetupFailure handles a reuse create that failed after the
// backend published the generation.
//
// It never deletes a shared generation blindly. It re-inspects first:
// only a generation this request owns that is still unadopted (created
// or stopped) is removed. A running or otherwise ambiguous generation may
// belong to a peer, so it is left in place and reported. Under
// CONTAINERGO_KEEP=1 no deletion is attempted at all and the handle is
// published only after its identity has been revalidated.
func reuseSetupFailure(ctx context.Context, cfg *config, image string, ctr *Container, cause error) (*Container, error) {
	if keepContainers() {
		verified, verifyErr := verifyRetainedHandle(ctx, ctr)
		if verifyErr != nil {
			return nil, withCleanupError(cause, verifyErr)
		}
		return verified, cause
	}
	if err := verifyReuseCleanupIdentity(ctx, cfg, image, ctr); err != nil {
		return nil, withCleanupError(cause, err)
	}
	cleanupErr := cleanupFailedCreate(ctx, cfg, cause, cause)
	return nil, withCleanupError(cause, cleanupErr)
}

// verifyReuseCleanupIdentity re-inspects a freshly created reuse
// generation before automatic cleanup and reports whether this request
// may still remove it. Anything ambiguous — a replaced generation, a
// foreign container, or one a peer may already have adopted — fails
// closed instead of authorizing a delete.
func verifyReuseCleanupIdentity(ctx context.Context, cfg *config, image string, ctr *Container) error {
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(verifyCtx, cfg.name)
	if err != nil {
		return fmt.Errorf("reuse %s: lock name: %w", cfg.name, err)
	}
	defer unlock()

	info, err := inspectNamed(verifyCtx, cfg, cfg.name)
	if err != nil {
		if isNotFoundFor(cfg.eng, cfg.name, err) {
			// Nothing left to clean up.
			return nil
		}
		return fmt.Errorf("reuse %s: verify generation before cleanup: %w", cfg.name, err)
	}
	if !failedCreateOwned(cfg, info) {
		return fmt.Errorf("reuse %s: refusing automatic cleanup of a generation this request does not own", cfg.name)
	}
	if info.labels[creationLabel] != ctr.creation {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, cfg.name)
	}
	if err := checkReuseOwned(info, image, cfg); err != nil {
		return fmt.Errorf("reuse %s: refusing automatic cleanup: %w", cfg.name, err)
	}
	switch info.state {
	case StateCreated, StateStopped:
		return nil
	default:
		return fmt.Errorf("reuse %s: %s generation may already be adopted; refusing automatic deletion",
			cfg.name, info.state)
	}
}

// deleteStoppedReuse removes a stopped reuse container only after
// verifying the managed, reuse, and creation labels on the inspected
// container. The critical section then re-inspects under the same
// per-name lock and refuses to delete unless the live generation is
// still the one observed, still stopped, and still owned: between the
// caller's poll and the lock a peer may have restarted this generation
// or replaced it. A replaced generation means another process already
// recreated the name; the caller loops and attaches to the fresh
// generation instead of deleting it.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	creation := info.labels[creationLabel]
	unlock, err := lockName(ctx, cfg.name)
	if err != nil {
		return fmt.Errorf("reuse %s: lock name: %w", cfg.name, err)
	}
	defer unlock()

	fresh, err := inspectNamed(ctx, cfg, cfg.name)
	if isNotFoundFor(cfg.eng, cfg.name, err) {
		// Already gone: nothing to replace.
		return nil
	}
	if err != nil {
		return fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
	}
	if err := checkReuseLabels(fresh, cfg); err != nil {
		// The live container is not the owned reuse generation the
		// caller observed (unlabeled or foreign replacement). Skip it
		// so the caller loops and attaches to whatever now owns the
		// name; never delete a container this request cannot prove.
		return nil
	}
	if actual := fresh.labels[creationLabel]; actual != creation {
		// A peer already replaced the name; let the caller attach to it.
		return nil
	}
	if fresh.state != StateStopped {
		// The generation is in use again; never force-delete a shared
		// container that a peer may have adopted.
		return nil
	}
	ctr := namedContainer(cfg, cfg.name)
	ctr.reused = true
	ctr.creation = creation
	// Delete by the backend's immutable ID when it reports one, so a
	// replacement created after this check is simply not found;
	// Apple Container is name-addressed and the lock still holds.
	target := fresh.uid
	if target == "" {
		target = cfg.name
	}
	return ctr.delete(ctx, target)
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
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
	return nil
}

func inspectNamed(ctx context.Context, cfg *config, id string) (*engineInfo, error) {
	return namedContainer(cfg, id).inspectFresh(ctx)
}

func namedContainer(cfg *config, id string) *Container {
	return &Container{
		id:        id,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
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

// checkReuseOwned reports whether an existing container may be adopted
// or deleted for this reuse request. All three labels are required: a
// reuse marker alone can be present on a foreign container, and a
// generation is what makes a later name-based delete safe.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	return nil
}

func checkReuseLabels(info *engineInfo, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: existing container has no ownership labels", cfg.name)
	}
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

func hasPublishedBinding(bound []boundPort, p publishSpec) bool {
	for _, b := range bound {
		if b.containerPort != p.containerPort || b.proto != p.proto {
			continue
		}
		if p.hostPort != 0 && b.hostPort != p.hostPort {
			continue
		}
		if p.hostAddr != "" && b.hostAddr != "" && b.hostAddr != p.hostAddr {
			continue
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
	return pruneListed(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group)
}
