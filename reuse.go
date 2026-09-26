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
		return nil, err
	}

	ctr := &Container{
		id:        base.id,
		runner:    base.runner,
		eng:       base.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		// Carry the creator bit: if this process created the generation, a
		// later failure must roll it back rather than hand back no handle at
		// all and leave a half-configured container with no way to remove it.
		creator:  base.creator,
		info:     info,
		creation: info.labels[creationLabel],
		uid:      info.uid,
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return ctr.rollbackResult(ctx, err)
	}
	fresh, err := ctr.inspectFresh(ctx)
	if err != nil {
		if ctr.uid != "" && isNotFoundFor(cfg.eng, err) {
			return nil, fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)
		}
		return nil, fmt.Errorf("reuse %s: verify before return: %w", cfg.name, err)
	}
	if err := verifyReuseResult(info, fresh, image, cfg); err != nil {
		return nil, err
	}
	return ctr, nil
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

// inspectReuseCandidate serializes Apple adoption with failed-create
// cleanup. A created generation is held under the name lock until it
// becomes running, so a cleanup that observed Created cannot delete it
// after a peer has begun adoption.
func inspectReuseCandidate(ctx context.Context, cfg *config) (*engineInfo, error) {
	if cfg.eng.name() != "apple" {
		return inspectNamed(ctx, cfg, cfg.name)
	}
	unlock, err := lockName(ctx, cfg.name)
	if err != nil {
		return nil, fmt.Errorf("reuse %s: lock name: %w", cfg.name, err)
	}
	defer unlock()
	for {
		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			return nil, err
		}
		switch info.state {
		case StateRunning, StateStopped:
			return info, nil
		case StateCreated, StateStopping, StateUnknown:
			if err := waitReusePoll(ctx); err != nil {
				return nil, err
			}
		default:
			if err := waitReusePoll(ctx); err != nil {
				return nil, err
			}
		}
	}
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

		info, err := inspectReuseCandidate(ctx, cfg)
		if err != nil {
			if !isNotFoundFor(cfg.eng, err) {
				return nil, err
			}
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			ctr, createErr := reuseCreate(context.WithoutCancel(ctx), image, cfg)
			if createErr != nil && ctr != nil && keepContainers() {
				return ctr, createErr
			}
			if createErr == nil {
				if ctr.info != nil && ctr.info.state == StateRunning {
					return ctr, nil
				}
				info, inspectErr := inspectReuseCandidate(ctx, cfg)
				if inspectErr != nil {
					if isNotFoundFor(cfg.eng, inspectErr) {
						continue
					}
					return nil, inspectErr
				}
				if info.state != StateRunning {
					continue
				}
				ctr.info = info
				ctr.creation = info.labels[creationLabel]
				ctr.uid = info.uid
				return ctr, nil
			}
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Only the primary operation branch is retryable; a cleanup
			// conflict must not make a failed create look like a peer win.
			primaryErr := primaryOperationError(createErr)
			if cfg.eng.nameConflict(primaryErr) || createRaceMissing(primaryErr) {
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
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
	}
	reaperRegistration, reaperBinary := preRegisterContainer(cfg)
	stdout, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		if !attempted {
			if reaperRegistration != nil {
				unregisterWithGlobalReaper(reaperRegistration)
			}
			return nil, err
		}
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) ||
			createRaceMissing(err) || createRaceMissing(classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			if reaperRegistration != nil {
				unregisterWithGlobalReaper(reaperRegistration)
			}
			return nil, err
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		if cleanupErr == nil && reaperRegistration != nil {
			unregisterWithGlobalReaper(reaperRegistration)
		}
		return nil, withCleanupError(classified, cleanupErr)
	}

	uid := cfg.eng.parseRunID(stdout)
	if cfg.eng.name() == "docker" && !validImmutableContainerID(cfg.eng, uid) {
		identityErr := fmt.Errorf("run %s: Docker run returned no valid immutable container ID", cfg.name)
		cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr)
		if cleanupErr == nil && reaperRegistration != nil {
			unregisterWithGlobalReaper(reaperRegistration)
		}
		return nil, withCleanupError(identityErr, cleanupErr)
	}

	ctr := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		// This call created the generation, so a post-create failure must
		// remove it rather than leave a half-configured container that the
		// next attach would adopt as ready.
		creator:  true,
		creation: cfg.creation,
		uid:      uid,
	}
	if reaperRegistration != nil {
		ctr.addReaperRegistration(reaperRegistration)
		completeContainerReaperRegistration(cfg, reaperRegistration, reaperBinary, ctr.uid)
	}
	if _, err := ctr.cachedInfo(ctx); err != nil {
		return ctr.rollbackResult(ctx, err)
	}
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return ctr.rollbackResult(ctx, err)
		}
	}
	// Reuse containers are shared and intentionally outlive the creating
	// process, so the registration is retired only once the generation is
	// fully configured. The reaper is EOF-gated crash insurance: retiring it
	// earlier would leave a half-configured container unreaped if this
	// process were killed during the copy loop.
	if reaperRegistration != nil {
		ctr.unregisterReapers()
	}
	return ctr, nil
}

// deleteStoppedReuse removes a stopped reuse container through a
// handle bound to its inspected generation, so the termination path
// re-checks the generation and deletes by immutable ID. A replaced
// generation means another process already recreated the name; the
// caller loops and attaches to the fresh generation instead of deleting it.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	// checkReuseLabels alone cannot tell one reuse group from another, so a
	// stopped container of a different group carrying this name must never
	// be deleted here.
	if err := checkReuseGroup(info, cfg); err != nil {
		return err
	}
	ctr := namedContainer(cfg, cfg.name)
	ctr.reused = true
	ctr.creation = info.labels[creationLabel]
	if requiresImmutableID(cfg.eng) {
		if !validImmutableContainerID(cfg.eng, info.uid) {
			return fmt.Errorf("reuse %s: stopped generation has no valid immutable ID", cfg.name)
		}
		ctr.uid = info.uid
	}
	_, err := ctr.terminateByName(ctx, info, true, func(fresh *engineInfo) error {
		if err := checkReuseLabels(fresh, cfg); err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, ErrGenerationReplaced) {
		return nil
	}
	return err
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

func verifyReuseResult(before, fresh *engineInfo, image string, cfg *config) error {
	if err := checkReuseCompat(fresh, image, cfg); err != nil {
		return err
	}
	if err := sameContainerIdentity(cfg.eng, before, fresh); err != nil {
		return fmt.Errorf("reuse %s: %w", cfg.name, err)
	}
	if before.state != StateRunning || fresh.state != StateRunning {
		return fmt.Errorf("reuse %s: state changed from %s to %s before return", cfg.name, before.state, fresh.state)
	}
	if before.image != fresh.image {
		return fmt.Errorf("reuse %s: image changed from %q to %q before return", cfg.name, before.image, fresh.image)
	}
	if !sameReusePorts(before.bound, fresh.bound) {
		return fmt.Errorf("reuse %s: published ports changed before return", cfg.name)
	}
	return nil
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
		reused:    cfg.reuse,
	}
}

// createRaceMissing reports only Apple's anchored concurrent-create form:
// a run command that reaches bootstrap and then loses the named object.
// Generic application output containing "not found" is deliberately excluded.
func createRaceMissing(err error) bool {
	var cliErrs []*cli.CLIError
	collectCLIErrors(err, &cliErrs)
	for _, cliErr := range cliErrs {
		if binary := normalizedCLIBinary(cliErr.Binary); binary != "" && binary != "container" {
			continue
		}
		if len(cliErr.Args) == 0 || strings.ToLower(cliErr.Args[0]) != "run" {
			continue
		}
		target := ""
		for i, arg := range cliErr.Args {
			if arg == "--name" && i+1 < len(cliErr.Args) {
				target = cliErr.Args[i+1]
				break
			}
		}
		if target == "" {
			continue
		}
		for _, rawLine := range strings.Split(cliErr.Stderr, "\n") {
			line := stripCLIErrorPrefix(rawLine)
			if appleTypedContainerIDNotFoundLine(line, target) {
				return true
			}
			for range 3 {
				if exactAppleIDNotFound(line, target) {
					return true
				}
				var rest string
				switch {
				case strings.HasPrefix(strings.ToLower(line), "failed to bootstrap container:"):
					rest = strings.TrimSpace(line[len("failed to bootstrap container:"):])
				case strings.HasPrefix(strings.ToLower(line), "failed to run container:"):
					rest = strings.TrimSpace(line[len("failed to run container:"):])
				}
				if rest == "" {
					break
				}
				line = rest
			}
		}
	}
	return false
}

// checkReuseOwned reports whether a stopped or running container may be
// adopted, deleted, or recreated for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if err := checkReuseLabels(info, cfg); err != nil {
		return err
	}
	if cfg.eng.name() == "docker" && !dockerIDRE.MatchString(info.uid) {
		return fmt.Errorf("reuse %s: existing container has no valid immutable ID", cfg.name)
	}
	if cfg.eng.name() == "apple" && info.uid != "" {
		return fmt.Errorf("reuse %s: existing container has an unexpected immutable ID", cfg.name)
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
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

// checkReuseGroup rejects a container that carries the requested name but
// belongs to a different reuse group. That container is another group's
// shared generation and must never be adopted or terminated by this call.
func checkReuseGroup(info *engineInfo, cfg *config) error {
	if cfg.reuseGroup == "" {
		return nil
	}
	if got := info.labels[reuseGroupLabel]; got != cfg.reuseGroup {
		return fmt.Errorf("reuse %s: existing container belongs to reuse group %q, want %q",
			cfg.name, got, cfg.reuseGroup)
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
	return pruneListed(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]pruneCandidate, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group, group)
}
