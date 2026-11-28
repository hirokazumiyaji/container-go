package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// reuseFlights collapses concurrent WithReuse get-or-create calls that
// share a name into one ensure operation. Each caller still applies its
// own compatibility check and wait strategy afterward.
var reuseFlights flightGroup[*Container]

var errReusePortPending = errors.New("reuse port binding is not ready")

type reuseValidationError struct{ error }

func (e *reuseValidationError) Unwrap() error { return e.error }

type reuseCleanupWarning struct {
	err error
}

func (w *reuseCleanupWarning) Error() string { return w.err.Error() }
func (w *reuseCleanupWarning) Unwrap() error { return w.err }

func (w *reuseCleanupWarning) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**reuseCleanupWarning)
	if !ok || w == nil {
		return false
	}
	*ptr = &reuseCleanupWarning{err: cli.WithRedactor(w.err, r)}
	return true
}

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

func (e *reuseCreateError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**reuseCreateError)
	if !ok || e == nil {
		return false
	}
	*ptr = &reuseCreateError{operation: cli.WithRedactor(e.operation, r), cleanup: cli.WithRedactor(e.cleanup, r)}
	return true
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

func (e *reuseIncompleteSetupError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**reuseIncompleteSetupError)
	if !ok || e == nil {
		return false
	}
	*ptr = &reuseIncompleteSetupError{err: cli.WithRedactor(e.err, r)}
	return true
}
func isReuseIncompleteSetupError(err error) bool {
	var incomplete *reuseIncompleteSetupError
	return errors.As(err, &incomplete)
}

// reuseFlightKey identifies one get-or-create request. Creation-time resource
// and security options are part of the identity: callers that ask for the same
// name with different settings must not share a leader's result. The order of
// capabilities, ulimits, extra hosts, and network aliases is irrelevant.
func reuseFlightKey(cfg *config, image string) string {
	parts := []string{cfg.eng.name(), cfg.eng.targetID(), cfg.name, image, cfg.platform, cfg.network, fmt.Sprint(cfg.networkExplicit), cfg.reuseGroup, cfg.runOptionsKey(), cfg.reuseConfigHash()}
	ports := make([]string, 0, len(cfg.exposed)+len(cfg.published))
	for _, port := range cfg.exposed {
		ports = append(ports, "expose="+port.String())
	}
	for _, port := range cfg.published {
		ports = append(ports, fmt.Sprintf("publish=%s:%d:%d/%s", port.HostIP(), port.HostPort, port.ContainerPort, port.Proto))
	}
	sort.Strings(ports)
	parts = append(parts, ports...)
	for i, part := range parts {
		parts[i] = strconv.Quote(part)
	}
	data, _ := json.Marshal(parts)
	return string(data)
}

// runOptionsKey returns a canonical, order-independent representation of the
// creation-time options that distinguish a reuse request.
func (c *config) runOptionsKey() string {
	parts := make([]string, 0, 2+len(c.capAdd)+len(c.capDrop)+len(c.ulimits)+len(c.extraHosts))
	if c.cpus > 0 {
		parts = append(parts, "cpus="+formatCPUs(c.cpus))
	}
	if !c.shmSize.IsZero() {
		// Hash the normalized byte count so equivalent spellings such as
		// "64M" and "67108864" share one flight.
		parts = append(parts, "shm-size="+strconv.FormatUint(c.shmSize.Bytes(), 10))
	}
	if c.privileged {
		parts = append(parts, "privileged")
	}
	if c.readOnlyRootFS {
		parts = append(parts, "read-only")
	}
	if c.initProcess {
		parts = append(parts, "init")
	}
	for _, server := range c.dnsServers {
		parts = append(parts, "dns="+server)
	}
	if c.stopSignal != "" {
		parts = append(parts, "stop-signal="+c.stopSignal)
	}
	if fingerprint := c.reuseEnvFingerprintValue(); fingerprint != "" {
		// The digest, never the raw environment values, keeps concurrent
		// callers that opted into environment matching on separate flights.
		parts = append(parts, "env-fingerprint="+fingerprint)
	}
	for _, capability := range c.capAdd {
		parts = append(parts, "cap-add="+capabilityKey(capability))
	}
	for _, capability := range c.capDrop {
		parts = append(parts, "cap-drop="+capabilityKey(capability))
	}
	for _, limit := range c.ulimits {
		parts = append(parts, "ulimit="+limit.arg())
	}
	for _, host := range c.extraHosts {
		parts = append(parts, "add-host="+host.arg())
	}
	if c.hostname != "" {
		parts = append(parts, "hostname="+c.hostname)
	}
	for _, alias := range c.networkAliases {
		parts = append(parts, "network-alias="+alias)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// PullAlways is a per-caller side effect, even when the container is
	// already running. Do it directly before joining the shared ensure
	// flight so every caller performs its own fetch instead of being
	// absorbed by an in-flight image pull.
	if cfg.pullPolicy == PullAlways {
		pullCtx, cancel := withDefaultTimeout(ctx, cfg.effectiveRunTimeout())
		defer cancel()
		if err := cfg.pullImage(pullCtx, image, cfg.platform); err != nil {
			return nil, err
		}
		cfg.imagePrepared = true
	}
	key := reuseFlightKey(cfg, image) + cfg.operationBudgetKey() + fmt.Sprintf("\x00attach=%d", cfg.effectiveReuseAttachTimeout())
	var leaderWarning error
	base, leader, err := reuseFlights.doWithLeader(ctx, key, func() (*Container, error) {
		// Do not detach a canceled preflight into a new shared flight.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.effectiveReuseAttachTimeout())
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

	// Mutable image tags may move during the shared ensure.
	resolvedImage, err := cfg.ensureImageRef(ctx, image)
	if err != nil {
		return nil, err
	}

	// The ensure result may have been inspected before a peer changed
	// network or port state. Once Docker has supplied an immutable ID,
	// every compatibility inspect must target that ID rather than the
	// reusable name.
	infoCtx, infoCancel := context.WithTimeout(ctx, cfg.effectiveReuseAttachTimeout())
	defer infoCancel()
	info, err := reuseInfoForCaller(infoCtx, cfg, base, resolvedImage, image)
	if err != nil {
		if errors.Is(err, ErrReuseMismatch) {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, joinEnvFileCleanupError(err, leaderWarning)
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("reuse %s: timed out waiting for complete inspect: %w", cfg.name, err)
		}
		var invalid *reuseValidationError
		if errors.As(err, &invalid) {
			return nil, joinEnvFileCleanupError(err, leaderWarning)
		}
		if leader && leaderWarning != nil {
			// The create itself succeeded and the only known warning is
			// cleanup. Preserve that usable handle even if the fresh
			// verification needed for compatibility cannot complete.
			return base, joinEnvFileCleanupError(err, leaderWarning)
		}
		return nil, err
	}

	if cfg.eng.capabilities().strictImageIdentity && resolvedImage.pinned {
		// A pinned reuse run requires an existing container to be
		// running the requested root digest. Docker enforces this via
		// inspect; Apple uses the same post-bootstrap probe on fresh
		// reuse adoption that verifyStrictCreatedImage closes for create.
		// The caller's compatibility inspect already described this
		// generation, so reuse it instead of spawning another inspect.
		probe := namedContainer(cfg, cfg.name)
		probe.image = resolvedImage
		if err := verifyStrictCreatedImage(ctx, probe, resolvedImage, info); err != nil {
			return nil, err
		}
	}

	containerImage := imageFromInfo(info)
	if resolvedImage.pinned {
		containerImage = resolvedImage
	}

	ctr := &Container{
		id:              base.id,
		runner:          cfg.runner,
		eng:             cfg.eng,
		queryTimeout:    cfg.queryTimeout,
		probeTimeout:    cfg.probeTimeout,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		reused:          true,
		created:         cfg.reusedCreated,
		creation:        info.labels[creationLabel],
		image:           containerImage,
		sync:            &lockPack{uid: info.uid, info: immutableInfo(info), defaultNetwork: info.defaultNetwork},
		dockerMeta:      cfg.dockerMeta,
	}
	ctr.applyConfigDiagnostics(cfg)
	// The leader's files are copied in reuseCreate. Every attaching
	// caller applies its own files after the shared generation is known.
	// A failed attach copy is reported without deleting the shared
	// container, which may be serving other callers.
	if !cfg.reusedCreated {
		if err := copyReuseFiles(ctx, ctr, cfg.files); err != nil {
			return nil, err
		}
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

	// A readiness wait can outlive the generation returned by the
	// ensure. Re-inspect by the immutable ID and repeat compatibility checks
	// before exposing the handle.
	fresh, err := ctr.inspectDynamicFresh(ctx)
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
	if err := checkReuseOwnedIdentity(fresh, resolvedImage, image, cfg); err != nil {
		return nil, err
	}
	if err := checkReuseCompatIdentity(fresh, resolvedImage, image, cfg); err != nil {
		return nil, err
	}
	startTestLogger(cfg, ctr)
	if err := runPostReadyHooks(ctx, ctr, cfg.postReadyHooks); err != nil {
		if cfg.reusedCreated {
			// The caller that created this generation owns its initialization.
			// Discard the generation so a later Run does not attach to a
			// container whose creator hook failed; peers detect the
			// replacement through ErrGenerationReplaced.
			err = cleanupReuseCreate(ctx, ctr, err)
		}
		return nil, joinEnvFileCleanupError(err, leaderWarning)
	}
	return ctr, nil
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller wait or port compatibility checks. Those run in
// reuseRun so every concurrent caller applies its own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false
	var lastInspectErr error
	var resolvedImage imageIdentity
	imageResolved := false
	resolveImage := func(resolveCtx context.Context) error {
		if imageResolved {
			return nil
		}
		var err error
		for {
			resolvedImage, err = cfg.ensureImageRef(resolveCtx, image)
			if err == nil {
				break
			}
			if retryErr := inspectRetryError(resolveCtx, err); retryErr != nil {
				return retryErr
			}
			if !isImageQueryDeadline(err) {
				return err
			}
			if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
				return err
			}
		}
		imageResolved = true
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil && !isNotFoundFor(cfg.eng, err) {
			if ctx.Err() != nil {
				return nil, reuseInspectWaitError(cfg.name, ctx.Err(), lastInspectErr)
			}
			if errors.Is(err, context.Canceled) {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			if err := inspectRetryError(ctx, err); err != nil {
				return nil, err
			}
			lastInspectErr = err
			if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			continue
		}
		lastInspectErr = nil
		exists := err == nil
		var state domain.State
		var ownedErr, imageErr error
		if exists {
			state = info.state
			// Resolve the image and verify its presence and digest identity
			// before the decision so the pure function only sees the result.
			if err := resolveImage(context.WithoutCancel(ctx)); err != nil {
				return nil, err
			}
			ownedErr = checkReuseOwnership(info, cfg)
			imageErr = ownedErr
			if ownedErr == nil {
				imageErr = checkReuseImageIdentity(info, resolvedImage, image, cfg)
			}
		}
		decision := domain.DecideReuse(domain.ReuseInput{
			Exists:            exists,
			State:             state,
			OwnershipVerified: ownedErr == nil,
			ImageCompatible:   imageErr == nil,
			Recreated:         recreated,
		})
		switch decision.Action {
		case domain.ReuseCreateNew:
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			// Resolve the caller's image immediately before create: a
			// tag might have changed while waiting, and we want the
			// immutable image that will actually be passed to run.
			if err := resolveImage(context.WithoutCancel(ctx)); err != nil {
				return nil, err
			}
			ctr, createErr := reuseCreateResolved(context.WithoutCancel(ctx), image, cfg, resolvedImage)
			if ctr != nil {
				// reuseCreate can return a usable handle together with a
				// post-create cleanup error. Do not turn that into an
				// orphaned container by treating it as a failed create.
				cfg.reusedCreated = true
				return ctr, createErr
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
				if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
					return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
				}
				continue
			}
			return nil, createErr
		case domain.ReuseStartExisting:
			if err := waitForReusePoll(ctx, cfg.effectiveReusePollInterval()); err != nil {
				return nil, reuseInspectWaitError(cfg.name, err, lastInspectErr)
			}
			continue
		case domain.ReuseRecreateIncompatible:
			// Only recycle containers this library created for reuse with a
			// compatible resolved image; never delete foreign leftovers. The
			// pure decision selects this only after ownership and image
			// identity matched above.
			if err := deleteStoppedReuse(ctx, cfg, info); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case domain.ReuseAttachRunning:
			ctr := &Container{
				id:              cfg.name,
				runner:          cfg.runner,
				eng:             cfg.eng,
				queryTimeout:    cfg.queryTimeout,
				probeTimeout:    cfg.probeTimeout,
				exposed:         cfg.exposed,
				published:       cfg.published,
				network:         cfg.network,
				networkExplicit: cfg.networkExplicit,
				reused:          true,
				creation:        info.labels[creationLabel],
				image:           resolvedImage,
				sync:            &lockPack{uid: info.uid, info: immutableInfo(info)},
				dockerMeta:      cfg.dockerMeta,
			}
			ctr.applyConfigDiagnostics(cfg)
			return ctr, nil
		case domain.ReuseFailMismatch:
			if ownedErr != nil {
				return nil, ownedErr
			}
			if imageErr != nil {
				return nil, imageErr
			}
			if exists && info.state == StateStopped && recreated {
				return nil, fmt.Errorf("reuse: %s", decision.Reason)
			}
			return nil, fmt.Errorf("reuse %s: %s", cfg.name, decision.Reason)
		default:
			return nil, fmt.Errorf("reuse %s: unexpected decision %q", cfg.name, decision.Action)
		}
	}
}

func reuseCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), cfg.effectiveRunTimeout())
	defer cancel()
	resolvedImage, err := cfg.ensureImageRef(runCtx, image)
	if err != nil {
		return nil, err
	}
	return reuseCreateResolved(ctx, image, cfg, resolvedImage)
}
