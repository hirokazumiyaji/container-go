package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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
func isReuseIncompleteSetupError(err error) bool {
	var incomplete *reuseIncompleteSetupError
	return errors.As(err, &incomplete)
}

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := cfg.eng.name() + "\x00" + cfg.name
	var leaderWarning error
	base, leader, err := reuseFlights.doWithLeader(ctx, key, func() (*Container, error) {
		// Do not detach a canceled preflight into a new shared flight.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
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

	// A creator's cached inspect is usable only for that creator. A waiter
	// must verify the named container with its own context before applying
	// compatibility and readiness checks; a warning result has no cached
	// inspect and therefore takes the fresh-inspect path for every caller.
	var info *engineInfo
	if leader && leaderWarning == nil && base.info != nil {
		info = base.info
	} else {
		info, err = inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if leader && leaderWarning != nil {
				// The create itself succeeded and the only known warning is
				// cleanup. Preserve that usable handle even if the fresh
				// verification needed for compatibility cannot complete.
				return base, joinEnvFileCleanupError(err, leaderWarning)
			}
			return nil, err
		}
	}
	if err := checkReuseCompat(info, image, cfg); err != nil {
		return nil, joinEnvFileCleanupError(err, leaderWarning)
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
	// The creator stops before readiness work when its env artifact is
	// still present. Waiters that received the successful shared result
	// still run their own compatibility/readiness path below.
	if leader && leaderWarning != nil {
		return ctr, leaderWarning
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, joinEnvFileCleanupError(err, leaderWarning)
	}
	if leader {
		return ctr, leaderWarning
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
			if ctr != nil {
				// reuseCreate can return a usable handle together with a
				// post-create cleanup error. Do not turn that into an
				// orphaned container by treating it as a failed create.
				return ctr, createErr
			}
			if createErr == nil {
				return ctr, nil
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

func reuseCreate(ctx context.Context, image string, cfg *config) (result *Container, retErr error) {
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
	stdout, _, runErr := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, image, envFile)...)
	envCleanupErr := cleanupEnvFileAfterUseContext(runCtx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}
	if runErr != nil {
		classified := cli.Classify(ctx, cfg.runner, runErr, cfg.eng.probe())
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
		cleanupFailedCreate(ctx, cfg, runErr, classified)
		return nil, joinEnvFileCleanupError(classified, envCleanupErr)
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
	// A warning with configured files must not publish the shared flight
	// before the required setup has completed. Finish the bounded inspect
	// and copy work first; any setup failure remains a failed flight.
	if _, err := ctr.cachedInfo(ctx); err != nil {
		cleanupErr := ctr.Terminate(context.WithoutCancel(ctx))
		if cleanupErr != nil {
			return ctr, joinEnvFileCleanupError(joinEnvFileCleanupError(err, envCleanupErr), cleanupErr)
		}
		return nil, joinEnvFileCleanupError(err, envCleanupErr)
	}
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			cleanupErr := ctr.Terminate(context.WithoutCancel(ctx))
			if cleanupErr != nil {
				return ctr, joinEnvFileCleanupError(joinEnvFileCleanupError(err, envCleanupErr), cleanupErr)
			}
			return nil, joinEnvFileCleanupError(err, envCleanupErr)
		}
	}
	if envCleanupErr != nil {
		return ctr, &reuseCleanupWarning{err: envCleanupErr}
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
