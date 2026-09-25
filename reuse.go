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

func reuseFailureResult(ctr *Container, err error) (*Container, error) {
	if keepContainers() {
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

	resolveCtx, cancel := context.WithTimeout(ctx, reuseAttachTimeout)
	defer cancel()
	stoppedRecreated := false
	for {
		if err := resolveCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("reuse %s: timed out waiting for a usable container", cfg.name)
			}
			return nil, err
		}

		base, err := reuseEnsureFlight(resolveCtx, image, cfg)
		if err != nil {
			// A retained failed create may carry a partial handle. Do not
			// discard it when CONTAINERGO_KEEP explicitly asks callers to
			// manage the retained generation themselves.
			if base != nil && keepContainers() {
				return base, err
			}
			return nil, err
		}
		info, err := inspectNamed(resolveCtx, cfg, cfg.name)
		if err != nil {
			if isNotFound(err) {
				cfg.clearReuseCreated()
				continue
			}
			return nil, err
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
			if err := deleteStoppedReuse(resolveCtx, cfg, info); err != nil {
				return nil, err
			}
			stoppedRecreated = true
			cfg.clearReuseCreated()
			continue
		case StateCreated, StateStopping, StateUnknown:
			if err := waitReusePoll(resolveCtx); err != nil {
				return nil, err
			}
			continue
		case StateRunning:
			// Continue below with a freshly inspected, running identity.
		default:
			if err := waitReusePoll(resolveCtx); err != nil {
				return nil, err
			}
			continue
		}

		if err := checkReuseCompat(info, image, cfg); err != nil {
			return nil, err
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
		// File application is deliberately outside the shared ensure
		// flight. Every caller, including the creator, applies its own
		// files after the generation is known and its identity has been
		// checked under the backend's safe target path.
		if err := copyReuseFiles(resolveCtx, ctr, cfg.files); err != nil {
			return reuseFailureResult(ctr, err)
		}
		if err := reuseWait(ctx, cfg, ctr); err != nil {
			return reuseFailureResult(ctr, err)
		}

		// A wait strategy or a concurrent replacement can outlive the
		// inspect above. Never return a handle for a different, stopped,
		// or otherwise changed generation.
		fresh, err := ctr.inspectFresh(ctx)
		if err != nil {
			return reuseFailureResult(ctr, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err))
		}
		if err := verifyReuseResult(info, fresh, image, cfg); err != nil {
			return reuseFailureResult(ctr, err)
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
	if !c.reusedCreated {
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
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller pull, file copy, wait, or port compatibility
// checks. Those run in reuseRun so every concurrent caller applies its
// own configuration.
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
			if !isNotFound(err) {
				return nil, err
			}
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			ctr, createErr := reuseCreate(context.WithoutCancel(ctx), image, cfg)
			if createErr == nil {
				cfg.reusedCreated = true
				cfg.reusedCreatedUID = ctr.uid
				cfg.reusedCreatedGeneration = ctr.creation
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
			if err := waitReusePoll(ctx); err != nil {
				return nil, err
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
	if !cfg.imagePrepared {
		if err := cfg.ensureImage(runCtx, image); err != nil {
			return nil, err
		}
	}
	stdout, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		if !attempted {
			return nil, err
		}
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
		return nil, withCleanupError(classified, cleanupErr)
	}

	ctr := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		creation:  cfg.creation,
		uid:       cfg.eng.parseRunID(stdout),
	}
	if _, err := ctr.cachedInfo(runCtx); err != nil {
		// A reuse container is shared as soon as its name is published;
		// do not let this caller remove a generation a peer may be using.
		return reuseFailureResult(ctr, err)
	}
	// WithFiles is applied by reuseRun after the shared ensure flight, not
	// here. A caller-specific copy failure must not poison waiters.
	return ctr, nil
}

type reuseCopyIdentity struct {
	uid      string
	creation string
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
	if c.uid != "" {
		info, err := c.inspectFresh(ctx)
		if err != nil {
			return reuseCopyIdentity{}, "", nil, err
		}
		if info.uid == "" || info.uid != c.uid {
			return reuseCopyIdentity{}, "", nil, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		if info.state != StateRunning {
			return reuseCopyIdentity{}, "", nil, fmt.Errorf("container state is %s before copy", info.state)
		}
		return reuseCopyIdentity{uid: c.uid}, c.uid, func() {}, nil
	}
	if !creationRE.MatchString(c.creation) {
		return reuseCopyIdentity{}, "", nil, fmt.Errorf("container has no valid creation generation")
	}
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return reuseCopyIdentity{}, "", nil, fmt.Errorf("lock name: %w", err)
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		unlock()
		return reuseCopyIdentity{}, "", nil, err
	}
	if info.state != StateRunning {
		unlock()
		return reuseCopyIdentity{}, "", nil, fmt.Errorf("container state is %s before copy", info.state)
	}
	if info.labels[creationLabel] != c.creation {
		unlock()
		return reuseCopyIdentity{}, "", nil, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	return reuseCopyIdentity{creation: c.creation}, c.id, unlock, nil
}

func (c *Container) verifyReuseCopyIdentity(ctx context.Context, identity reuseCopyIdentity) error {
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return err
	}
	if identity.uid != "" {
		if info.uid == "" || info.uid != identity.uid {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		return nil
	}
	if info.labels[creationLabel] == "" || info.labels[creationLabel] != identity.creation {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	return nil
}

// deleteStoppedReuse removes a stopped reuse container only after
// rechecking its state and generation under the backend's delete guard.
// A generation that became running may already have been adopted by a
// peer, so it is left in place and the caller re-inspects instead.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = info.labels[creationLabel]

	if usesNameAddressedDeletes(cfg.eng) {
		guardCtx, guardCancel := withDefaultTimeout(ctx, queryTimeout)
		defer guardCancel()
		unlock, err := lockName(guardCtx, cfg.name)
		if err != nil {
			return fmt.Errorf("reuse %s: lock stopped generation: %w", cfg.name, err)
		}
		defer unlock()
		fresh, err := ctr.inspectFresh(guardCtx)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
		}
		if err := checkReuseLabels(fresh, cfg); err != nil {
			return nil
		}
		if !sameStoppedGeneration(info, fresh) || fresh.state != StateStopped {
			return nil
		}
		if fresh.uid != "" {
			ctr.uid = fresh.uid
		}
		return ctr.delete(guardCtx, ctr.deleteTarget())
	}

	fresh, err := ctr.inspectFresh(ctx)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
	}
	if err := checkReuseLabels(fresh, cfg); err != nil {
		return nil
	}
	if !sameStoppedGeneration(info, fresh) || fresh.state != StateStopped {
		return nil
	}
	if fresh.uid != "" {
		ctr.uid = fresh.uid
	}
	return ctr.delete(ctx, ctr.deleteTarget())
}

func sameStoppedGeneration(before, fresh *engineInfo) bool {
	if before == nil || fresh == nil || before.labels[creationLabel] == "" ||
		before.labels[creationLabel] != fresh.labels[creationLabel] {
		return false
	}
	return before.uid == "" || before.uid == fresh.uid
}

func (c *Container) deleteTarget() string {
	if c.uid != "" {
		return c.uid
	}
	return c.id
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
	if before.uid != "" || fresh.uid != "" {
		return before.uid != "" && before.uid == fresh.uid
	}
	return before.labels[creationLabel] != "" && before.labels[creationLabel] == fresh.labels[creationLabel]
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

// platformSelectorMatches compares the fields selected by the caller.
// An omitted selector component is a wildcard; every component that the
// caller does specify must be present in the backend's normalized value.
func platformSelectorMatches(requested, actual string) bool {
	wantOS, wantArch, wantVariant := splitPlatform(strings.TrimSpace(requested))
	gotOS, gotArch, gotVariant := splitPlatform(strings.TrimSpace(actual))
	return (wantOS == "" || strings.EqualFold(wantOS, gotOS)) &&
		(wantArch == "" || strings.EqualFold(wantArch, gotArch)) &&
		(wantVariant == "" || strings.EqualFold(wantVariant, gotVariant))
}

func samePlatformMetadata(a, b string) bool {
	aOS, aArch, aVariant := splitPlatform(strings.TrimSpace(a))
	bOS, bArch, bVariant := splitPlatform(strings.TrimSpace(b))
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
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	if cfg.platform != "" && !platformSelectorMatches(cfg.platform, info.platform) {
		if info.platform == "" {
			return fmt.Errorf("reuse %s: platform %q could not be verified", cfg.name, cfg.platform)
		}
		return fmt.Errorf("reuse %s: platform %q does not match existing %q", cfg.name, cfg.platform, info.platform)
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
