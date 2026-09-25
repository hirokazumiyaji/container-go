package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	infoCtx, infoCancel := context.WithTimeout(ctx, reuseAttachTimeout)
	defer infoCancel()
	info, err := reuseInfoForCaller(infoCtx, cfg, base.info)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("reuse %s: timed out waiting for complete inspect", cfg.name)
		}
		return nil, err
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
		creation:  info.labels[creationLabel],
		uid:       base.uid,
	}
	ctr.cacheInfo(info)
	if err := reuseWait(ctx, cfg, ctr); err != nil {
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
			if isNotFound(err) {
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
					if !waitReusePoll(ctx) {
						return nil, reuseContextError(ctx, cfg.name)
					}
					continue
				}
				return nil, createErr
			}
			// Inspect can race a peer creating/replacing the container, or
			// the backend can briefly lose its daemon. Keep the attach
			// resolution bounded instead of failing on the first transient
			// or incomplete response.
			if !retryReuseInspect(err) {
				return nil, err
			}
			if !waitReusePoll(ctx) {
				return nil, reuseContextError(ctx, cfg.name)
			}
			continue
		}

		switch info.state {
		case StateCreated, StateRestarting, StateUnknown:
			if !waitReusePoll(ctx) {
				return nil, reuseContextError(ctx, cfg.name)
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
			if !reuseInfoComplete(cfg, info) {
				if !waitReusePoll(ctx) {
					return nil, reuseContextError(ctx, cfg.name)
				}
				continue
			}
			ctr := &Container{
				id:        cfg.name,
				runner:    cfg.runner,
				eng:       cfg.eng,
				exposed:   cfg.exposed,
				published: cfg.published,
				reused:    true,
				creation:  info.labels[creationLabel],
			}
			ctr.cacheInfo(info)
			return ctr, nil
		default:
			if !waitReusePoll(ctx) {
				return nil, reuseContextError(ctx, cfg.name)
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
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		creation:  cfg.creation,
		uid:       cfg.eng.parseRunID(stdout),
	}
	infoCtx, infoCancel := context.WithTimeout(ctx, reuseAttachTimeout)
	info, err := reuseInfoForCaller(infoCtx, cfg, ctr.info)
	infoCancel()
	if err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	ctr.cacheInfo(info)
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
	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = info.labels[creationLabel]
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

// reuseInfoComplete reports whether inspect contains all connection data
// needed by this request. A Running status alone is not enough: Apple can
// report a container before its network address appears, and Docker can
// report it before an exposed port receives a host binding.
func reuseInfoComplete(cfg *config, info *engineInfo) bool {
	if info == nil {
		return false
	}
	if len(cfg.exposed) == 0 && len(cfg.published) == 0 {
		return true
	}
	ctr := &Container{eng: cfg.eng, exposed: cfg.exposed, published: cfg.published}
	if !ctr.infoComplete(info) {
		return false
	}
	for _, p := range cfg.published {
		if !hasPublishedBinding(info.bound, p) {
			return false
		}
	}
	return true
}

// reuseInfoReady additionally requires the lifecycle state to be running.
// A complete Created inspect still lacks a usable container and must be
// refreshed before WithReuse publishes a handle.
func reuseInfoReady(cfg *config, info *engineInfo) bool {
	return info != nil && info.state == StateRunning && reuseInfoComplete(cfg, info)
}

// retryReuseInspect keeps a launch failure permanent while allowing
// transient daemon/CLI failures to be retried within the attach budget.
func retryReuseInspect(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var launchErr *exec.Error
	return !errors.As(err, &launchErr)
}

// reuseInfoForCaller fills a missing/incomplete inspect without turning a
// backend's brief inspect failure into a failed WithReuse call. The initial
// value is usually the shared ensure's cached complete inspect; the loop is
// primarily for callers that received a Running result before its endpoint
// data became visible.
func reuseInfoForCaller(ctx context.Context, cfg *config, initial *engineInfo) (*engineInfo, error) {
	if reuseInfoReady(cfg, initial) {
		return initial, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err == nil {
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
		}
		if err != nil && isNotFound(err) {
			return nil, err
		}
		if err != nil && !retryReuseInspect(err) {
			return nil, err
		}
		if !waitReusePoll(ctx) {
			return nil, ctx.Err()
		}
	}
}

func waitReusePoll(ctx context.Context) bool {
	timer := time.NewTimer(reusePollInterval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func reuseContextError(ctx context.Context, name string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container", name)
	}
	return ctx.Err()
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

// checkReuseOwned reports whether a stopped container may be deleted
// and recreated for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
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
