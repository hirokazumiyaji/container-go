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

func waitReuseReady(ctx context.Context, cfg *config, ctr *Container) (*engineInfo, error) {
	readyCtx, cancel := withMaxTimeout(ctx, reuseAttachTimeout)
	defer cancel()
	for {
		info, err := ctr.inspectFresh(readyCtx)
		if err != nil {
			return nil, err
		}
		if info.state == StateRunning && reusePortsReady(info, cfg) {
			return info, nil
		}
		if err := waitReusePoll(readyCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("reuse %s: timed out waiting for running ports: %w", cfg.name, err)
			}
			return nil, err
		}
	}
}

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

	creation := base.creation
	if creation == "" && base.info != nil {
		creation = base.info.labels[creationLabel]
	}
	ctr := &Container{
		id:        base.id,
		runner:    base.runner,
		eng:       base.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		info:      base.info,
		creation:  creation,
		uid:       base.uid,
	}
	info := base.info
	if info == nil {
		info, err = ctr.inspectFresh(ctx)
		if err != nil {
			return nil, err
		}
	}
	// The ensure path already waited for running state. Only perform an
	// additional poll when the list-time snapshot does not yet contain all
	// requested bindings; this preserves the baseline inspect contract for
	// ordinary ready containers while closing the post-start binding race.
	if !reusePortsReady(info, cfg) {
		info, err = waitReuseReady(ctx, cfg, ctr)
		if err != nil {
			return nil, err
		}
	}
	if err := checkReuseCompat(info, image, cfg); err != nil {
		return nil, err
	}
	ctr.mu.Lock()
	ctr.info = info
	ctr.mu.Unlock()
	ctr.inspectMu.Lock()
	ctr.creation = info.labels[creationLabel]
	ctr.uid = info.uid
	ctr.inspectMu.Unlock()
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, err
	}
	// Readiness can outlive the generation inspected by the ensure path.
	// Re-inspect immediately before returning and refuse to hand out a
	// handle for a different, unowned, or no-longer-running object. Apple
	// keeps the stable name lock across this final inspect so the reaper
	// cannot delete and recreate the name between verification and
	// publication of the handle.
	finalTimeout := reuseFinalVerifyTimeout
	if reuseAttachTimeout > 0 && reuseAttachTimeout < finalTimeout {
		finalTimeout = reuseAttachTimeout
	}
	finalCtx, finalCancel := withMaxTimeout(ctx, finalTimeout)
	defer finalCancel()
	unlock, err := lockReuseFinal(finalCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
	}
	defer unlock()
	fresh, err := ctr.inspectFreshLocked(finalCtx)
	if err != nil {
		// A Docker handle with a resolved immutable ID that disappears
		// from its final inspect is necessarily no longer the generation
		// observed before readiness. Report that replacement explicitly;
		// never fall back to inspecting the logical name.
		if ctr.uid != "" && isNotFound(err) {
			return nil, fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
		}
		return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
	}
	if err := verifyReuseResult(info, fresh, image, cfg); err != nil {
		return nil, err
	}
	return ctr, nil
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
			if !isNotFound(err) {
				return nil, err
			}
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
				if err := waitReusePoll(ctx); err != nil {
					return nil, err
				}
				continue
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
			if err := deleteStoppedReuseWithImage(ctx, cfg, info, image); err != nil {
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
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
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
	if err := reusePostCreateWithImage(ctx, cfg, ctr, image); err != nil {
		// The successful run has already published this generation by
		// name. A peer may have attached to it, so a failed local inspect,
		// identity check, or copy is not proof that this caller owns the
		// only reference. Leave the shared generation in place for the
		// next reuse attempt.
		return nil, err
	}
	return ctr, nil
}

// reusePostCreate verifies and initializes a newly-created reuse
// generation. A successful `run` can return before Apple reaches running
// state or publishes all requested port bindings, so wait for both before
// establishing the baseline used by the caller's compatibility check.
//
//nolint:unused // retained for package callers using the original post-create helper
func reusePostCreate(ctx context.Context, cfg *config, ctr *Container) error {
	return reusePostCreateWithImage(ctx, cfg, ctr, "")
}

func reusePostCreateWithImage(ctx context.Context, cfg *config, ctr *Container, image string) error {
	postCtx, cancel := withMaxTimeout(ctx, reuseAttachTimeout)
	defer cancel()

	var ready *engineInfo
	for {
		info, err := ctr.inspectFresh(postCtx)
		if err != nil {
			return err
		}
		if err := verifyReusePostCreateInfo(cfg, ctr, info); err != nil {
			return err
		}
		if image != "" && cfg.reuse {
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return err
			}
		}
		if info.state == StateRunning && reusePortsReady(info, cfg) {
			ready = info
			break
		}
		if err := waitReusePoll(postCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("reuse %s: timed out waiting for post-create readiness: %w", cfg.name, err)
			}
			return err
		}
	}

	// Recheck under the Apple name lock and keep it through all file copies.
	// The baseline and the copy therefore cannot target different
	// generations. Docker is already bound to its immutable UID.
	target := ready.uid
	if cfg.eng.name() == "apple" {
		unlock, err := lockName(postCtx, cfg.name)
		if err != nil {
			return fmt.Errorf("reuse %s: lock post-create generation: %w", cfg.name, err)
		}
		defer unlock()
		fresh, err := ctr.inspectFreshLocked(postCtx)
		if err != nil {
			return err
		}
		if err := verifyReusePostCreateInfo(cfg, ctr, fresh); err != nil {
			return err
		}
		if image != "" && cfg.reuse {
			if err := checkReuseOwned(fresh, image, cfg); err != nil {
				return err
			}
		}
		if fresh.state != StateRunning || !reusePortsReady(fresh, cfg) {
			return fmt.Errorf("reuse %s: post-create generation changed before initialization", cfg.name)
		}
		if err := sameContainerIdentity(cfg.eng, ready, fresh); err != nil {
			return fmt.Errorf("reuse %s: %w", cfg.name, err)
		}
		ready = fresh
		target = cfg.name
		for _, f := range cfg.files {
			if err := ctr.copyToContainerTarget(postCtx, target, f.HostPath, f.ContainerPath); err != nil {
				// The shared generation remains available to peers; a
				// caller-specific copy failure must not delete it.
				return err
			}
		}
	} else {
		if !validImmutableID(cfg.eng, target) || target != ctr.uid {
			return fmt.Errorf("reuse %s: %w: post-create Docker ID changed", cfg.name, ErrGenerationReplaced)
		}
		for _, f := range cfg.files {
			if err := ctr.copyToContainerTarget(postCtx, target, f.HostPath, f.ContainerPath); err != nil {
				return err
			}
		}
	}

	ctr.mu.Lock()
	ctr.info = ready
	ctr.mu.Unlock()
	return nil
}

func verifyReusePostCreateInfo(cfg *config, ctr *Container, info *engineInfo) error {
	if info == nil || info.labels[managedLabel] != "true" ||
		(cfg.reuse && info.labels[reuseLabel] != "true") ||
		!validCreationID(cfg.creation) || info.labels[creationLabel] != cfg.creation {
		return fmt.Errorf("reuse %s: %w: post-create generation could not be verified", cfg.name, ErrGenerationReplaced)
	}
	if requiresImmutableID(cfg.eng) && (!validImmutableID(cfg.eng, info.uid) || info.uid != ctr.uid) {
		return fmt.Errorf("reuse %s: %w: post-create Docker ID changed", cfg.name, ErrGenerationReplaced)
	}
	return nil
}

func reusePortsReady(info *engineInfo, cfg *config) bool {
	if info == nil {
		return false
	}
	if !cfg.eng.directIP() {
		for _, spec := range cfg.exposed {
			if !hasBoundPort(info.bound, spec.port, spec.proto) {
				return false
			}
		}
	}
	for _, p := range cfg.published {
		if !hasPublishedBinding(info.bound, p) {
			return false
		}
	}
	return true
}

// deleteStoppedReuse removes a stopped reuse container only after
// verifying the managed, reuse, and creation labels on the inspected
// container. The image-aware production path additionally verifies the
// requested image. A fresh inspect must still be StateStopped; a
// same-generation running replacement is therefore never deleted.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	return deleteStoppedReuseWithImage(ctx, cfg, info, "")
}

func deleteStoppedReuseWithImage(ctx context.Context, cfg *config, info *engineInfo, image string) error {
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = info.labels[creationLabel]
	if image != "" {
		if err := checkReuseOwned(info, image, cfg); err != nil {
			return err
		}
	}
	_, err := ctr.terminateByNameWithImage(ctx, info, true, image)
	if errors.Is(err, ErrGenerationReplaced) {
		// A different generation means the name was recreated. Let the
		// ensure loop inspect and attach to that fresh generation.
		return nil
	}
	return err
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

// verifyReuseResult applies the final ownership and identity checks after
// readiness. The initial inspect is part of the comparison: a fresh
// inspect alone cannot tell whether a backend returned a different object
// with compatible labels.
func verifyReuseResult(before, fresh *engineInfo, image string, cfg *config) error {
	// Recheck the live ownership/configuration first so an unowned or
	// incompatible replacement gets the same fail-closed treatment as the
	// initial attach check.
	if err := checkReuseCompat(fresh, image, cfg); err != nil {
		return err
	}
	if err := sameContainerIdentity(cfg.eng, before, fresh); err != nil {
		return fmt.Errorf("reuse %s: %w", cfg.name, err)
	}
	if before.state != StateRunning || fresh.state != StateRunning {
		return fmt.Errorf("reuse %s: state changed from %s to %s before return", cfg.name, before.state, fresh.state)
	}
	if fresh.image != before.image {
		return fmt.Errorf("reuse %s: image changed from %q to %q before return", cfg.name, before.image, fresh.image)
	}
	if !sameEnginePlatform(before, fresh) {
		return fmt.Errorf("reuse %s: platform changed from %q to %q before return", cfg.name, before.platform, fresh.platform)
	}
	if !sameReusePorts(before.bound, fresh.bound) {
		return fmt.Errorf("reuse %s: published ports changed before return", cfg.name)
	}
	return nil
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
	if cfg.eng.name() != "apple" {
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

// checkReuseOwned reports whether an existing container may be adopted
// or deleted for this reuse request. All three labels are required: a
// reuse marker alone can be present on a foreign container, and a
// generation is what makes a later name-based delete safe.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: inspect returned no container identity", cfg.name)
	}
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	if requiresImmutableID(cfg.eng) && !validImmutableID(cfg.eng, info.uid) {
		return fmt.Errorf("reuse %s: existing container has no valid immutable ID", cfg.name)
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
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

func checkReuseLabels(info *engineInfo, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: inspect returned no container identity", cfg.name)
	}
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if info.labels[managedLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container is not managed by container-go", cfg.name)
	}
	if !creationRE.MatchString(info.labels[creationLabel]) {
		return fmt.Errorf("reuse %s: existing container has no valid creation generation: %w", cfg.name, ErrGenerationReplaced)
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
		if b.containerPort != p.containerPort || b.proto != p.proto || b.hostPort <= 0 {
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
	return pruneListedWithGroup(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group, group)
}
