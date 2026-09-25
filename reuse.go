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
		return nil, err
	}

	info := base.info
	if info == nil {
		info, err = inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			return nil, err
		}
	}
	// A shared ensure may have been performed by a caller that did not
	// request a platform. Resolve this caller's requested OCI platform
	// from a copy, rather than trusting or mutating the shared snapshot.
	info, err = resolveInfoPlatform(ctx, cfg.eng, cfg.runner, info, cfg.platform)
	if err != nil {
		return nil, fmt.Errorf("reuse %s: resolve existing platform: %w", cfg.name, err)
	}
	if err := checkReuseCompat(info, image, cfg); err != nil {
		return nil, err
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
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, err
	}
	// A readiness strategy can outlive the generation inspected above.
	// Apple has no immutable ID, so hold the cooperating-process name
	// lock across the final inspect and publication of the handle. The
	// lock cannot cover an uncooperating external delete/recreate, but it
	// closes the library-level final-inspect race; every later name
	// operation repeats the generation check.
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
	// Publish the exact inspected identity on the returned handle. Docker
	// is now bound to its full UID; Apple retains the generation label
	// that subsequent operations verify.
	ctr.mu.Lock()
	ctr.info = fresh
	ctr.mu.Unlock()
	ctr.inspectMu.Lock()
	ctr.creation = fresh.labels[creationLabel]
	ctr.uid = fresh.uid
	ctr.bootstrap = false
	ctr.inspectMu.Unlock()
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
				time.Sleep(reusePollInterval)
				continue
			}
			return nil, createErr
		}

		if err := validateReuseIdentity(cfg.eng, info); err != nil {
			return nil, fmt.Errorf("reuse %s: %w: %v", cfg.name, ErrGenerationReplaced, err)
		}
		switch info.state {
		case StateCreated, StateStopping, StateUnknown:
			time.Sleep(reusePollInterval)
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
			return &Container{
				id:                cfg.name,
				runner:            cfg.runner,
				eng:               cfg.eng,
				exposed:           cfg.exposed,
				published:         cfg.published,
				reused:            true,
				info:              info,
				creation:          info.labels[creationLabel],
				uid:               info.uid,
				requestedPlatform: cfg.platform,
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
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(err) || createRaceMissing(classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			return nil, err
		}
		cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, classified
	}

	ctr := &Container{
		id:                cfg.name,
		runner:            cfg.runner,
		eng:               cfg.eng,
		exposed:           cfg.exposed,
		published:         cfg.published,
		reused:            true,
		creation:          cfg.creation,
		uid:               cfg.eng.parseRunID(stdout),
		requestedPlatform: cfg.platform,
		bootstrap:         true,
	}
	if _, err := ctr.cachedInfo(ctx); err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	ctr.inspectMu.Lock()
	ctr.bootstrap = false
	ctr.inspectMu.Unlock()
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			_ = ctr.Terminate(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return ctr, nil
}

// deleteStoppedReuse removes a stopped reuse container through a
// handle bound to its inspected generation, so Terminate re-checks the
// generation and deletes by immutable ID. A replaced generation means
// another process already recreated the name; the caller loops and
// attaches to the fresh generation instead of deleting it.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	if info == nil {
		return generationReplaced(cfg.name)
	}
	if cfg.eng.name() == "docker" && info.uid != "" && !validDockerUID(info.uid) {
		return generationReplaced(cfg.name)
	}
	if cfg.eng.name() == "apple" && info.uid != "" {
		return generationReplaced(cfg.name)
	}
	ctr := &Container{
		id:                cfg.name,
		runner:            cfg.runner,
		eng:               cfg.eng,
		reused:            true,
		creation:          info.labels[creationLabel],
		uid:               info.uid,
		requestedPlatform: cfg.platform,
		bootstrap:         cfg.eng.name() == "docker" && !validDockerUID(info.uid),
	}
	err := ctr.Terminate(ctx)
	if errors.Is(err, ErrGenerationReplaced) {
		return nil
	}
	return err
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
	if cfg.waitStrategy == nil {
		return nil
	}
	if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: ctr}); err != nil {
		waitErr := fmt.Errorf("reuse %s failed to become ready: %w", ctr.id, err)
		// A strategy may itself have observed the replacement. Do not
		// issue a name-based diagnostic read in that case; retain both
		// the historical readiness prefix and the stable sentinel.
		if errors.Is(err, ErrGenerationReplaced) || isNotFound(err) {
			return errors.Join(waitErr, generationReplaced(ctr.id))
		}

		unlock, verifyErr := verifyReuseWaitIdentity(context.WithoutCancel(ctx), ctr)
		if verifyErr != nil {
			if errors.Is(verifyErr, ErrGenerationReplaced) || isNotFound(verifyErr) {
				return errors.Join(waitErr, verifyErr)
			}
			// An identity that cannot be verified is not safe to diagnose
			// with a name-based log read either.
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
		ctr.inspectMu.RLock()
		creation := ctr.creation
		ctr.inspectMu.RUnlock()
		unlock, err := lockName(ctx, ctr.id)
		if err != nil {
			return noop, fmt.Errorf("lock name: %w", err)
		}
		info, err := ctr.inspectFreshLocked(ctx)
		if err != nil {
			unlock()
			return noop, err
		}
		if !sameEngineIdentity(ctr.eng, &engineInfo{labels: map[string]string{creationLabel: creation}}, info) {
			unlock()
			return noop, generationReplaced(ctr.id)
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
	ctr.inspectMu.RLock()
	uid := ctr.uid
	ctr.inspectMu.RUnlock()
	if !validDockerUID(uid) {
		return noop, generationReplaced(ctr.id)
	}
	info, err := ctr.inspectFreshLocked(ctx)
	if err != nil {
		return noop, err
	}
	if !sameEngineIdentity(ctr.eng, &engineInfo{uid: uid}, info) {
		return noop, generationReplaced(ctr.id)
	}
	if err := checkFreshReusePlatform(ctr, info); err != nil {
		return noop, err
	}
	return noop, nil
}

func checkFreshReusePlatform(ctr *Container, info *engineInfo) error {
	if ctr.requestedPlatform == "" || platformMatches(ctr.requestedPlatform, info.platform) {
		return nil
	}
	return fmt.Errorf("reuse %s: fresh platform %q does not match requested %q", ctr.id, info.platform, ctr.requestedPlatform)
}

func verifyReuseResult(before, fresh *engineInfo, image string, cfg *config) error {
	if !sameEngineIdentity(cfg.eng, before, fresh) {
		return fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
	}
	if before.imageID != "" || fresh.imageID != "" {
		if before.imageID == "" || fresh.imageID == "" || before.imageID != fresh.imageID {
			return fmt.Errorf("reuse %s: image identity changed before return", cfg.name)
		}
	}
	if fresh.state != before.state {
		return fmt.Errorf("reuse %s: state changed from %s to %s before return", cfg.name, before.state, fresh.state)
	}
	if fresh.state != StateRunning {
		return fmt.Errorf("reuse %s: state changed to %s before return", cfg.name, fresh.state)
	}
	if fresh.image != before.image {
		return fmt.Errorf("reuse %s: image changed from %q to %q before return", cfg.name, before.image, fresh.image)
	}
	if !samePlatform(before.platform, fresh.platform) {
		return fmt.Errorf("reuse %s: platform changed from %q to %q before return", cfg.name, before.platform, fresh.platform)
	}
	if cfg.platform != "" && !platformMatches(cfg.platform, fresh.platform) {
		return fmt.Errorf("reuse %s: platform %q does not match fresh OCI identity %q", cfg.name, cfg.platform, fresh.platform)
	}
	if !sameReusePorts(before.bound, fresh.bound) {
		return fmt.Errorf("reuse %s: published ports changed before return", cfg.name)
	}
	return checkReuseCompat(fresh, image, cfg)
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

func lockReuseFinal(ctx context.Context, cfg *config) (func(), error) {
	if cfg.eng.name() == "docker" {
		return func() {}, nil
	}
	return lockName(ctx, cfg.name)
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

// checkReuseOwned reports whether a stopped container may be deleted
// and recreated for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: %w: existing container inspect returned no identity", cfg.name, ErrGenerationReplaced)
	}
	if err := validateReuseIdentity(cfg.eng, info); err != nil {
		return fmt.Errorf("reuse %s: %w: %v", cfg.name, ErrGenerationReplaced, err)
	}
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	if cfg.platform != "" {
		if info.platform == "" {
			return fmt.Errorf("reuse %s: platform %q could not be verified", cfg.name, cfg.platform)
		}
		if !platformMatches(cfg.platform, info.platform) {
			return fmt.Errorf("reuse %s: platform %q does not match existing %q", cfg.name, cfg.platform, info.platform)
		}
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
