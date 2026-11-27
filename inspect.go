package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/domain"
	"math"
)

// State is a container lifecycle state.
type State = domain.State

// verifyCreatedOwnership validates the labels and backend identity before
// an external reaper is allowed to retain a newly created container. A
// successful `run` command alone is not proof that the library's ownership
// labels were attached. The returned inspect describes the verified
// generation so callers can reuse it instead of spawning another inspect.
func verifyCreatedOwnership(ctx context.Context, c *Container, cfg *config) (*engineInfo, error) {
	if c == nil || cfg == nil || !validCreationID(c.creation) {
		return nil, fmt.Errorf("verify created container: invalid creation generation")
	}
	verifyCtx, cancel := withDefaultTimeout(ctx, cfg.effectiveQueryTimeout())
	defer cancel()
	caps := cfg.eng.capabilities()

	var info *engineInfo
	for {
		var err error
		inspectContainer := c
		if caps.immutableID {
			uid := c.immutableID()
			if !domain.IsImmutableContainerID(uid) {
				return nil, fmt.Errorf("verify created container %s: invalid immutable container ID %q", c.id, uid)
			}
			inspectContainer = &Container{id: uid, runner: c.runner, eng: c.eng, queryTimeout: c.queryTimeout, probeTimeout: c.probeTimeout, sync: &lockPack{uid: uid}}
		}
		unlock := func() {}
		if caps.nameAddressed {
			unlock, err = lockName(verifyCtx, c.id)
			if err != nil {
				return nil, fmt.Errorf("verify created container %s: lock name: %w", c.id, err)
			}
		}
		if caps.nameAddressed {
			info, err = inspectContainer.inspectFreshLocked(verifyCtx)
		} else {
			info, err = inspectContainer.inspectFresh(verifyCtx)
		}
		if unlock != nil {
			unlock()
		}
		if err == nil {
			break
		}
		if !isNotFoundFor(cfg.eng, err) {
			if err := inspectRetryError(verifyCtx, err); err != nil {
				return nil, fmt.Errorf("verify created container %s: %w", c.id, err)
			}
		}
		if err := waitForReusePoll(verifyCtx, cfg.effectiveReusePollInterval()); err != nil {
			return nil, fmt.Errorf("verify created container %s: %w", c.id, err)
		}
	}
	if info == nil {
		return nil, fmt.Errorf("verify created container %s: inspect returned no container", c.id)
	}
	expected := domain.OwnershipExpected{
		RequireManaged: true,
		Session:        sessionID(),
		Creation:       c.creation,
		RequireReuse:   cfg.reuse,
		ReuseGroup:     cfg.reuseGroup,
	}
	if caps.immutableID {
		expected.UID = c.immutableID()
		expected.RequireImmutableID = true
	} else {
		expected.RequireNoImmutableID = true
	}
	if failure := domain.VerifyOwnership(expected, ownershipObserved(info)); failure != domain.OwnershipFailureNone {
		return nil, fmt.Errorf("verify created container %s: %s", c.id, createdOwnershipFailure(failure, caps.immutableID))
	}
	c.pack().mu.Lock()
	c.pack().info = info
	c.pack().mu.Unlock()
	c.setImmutableID(info.uid)
	// Return a copy: pack().info is owned under mu, and reuse callers attach
	// daemon network metadata to the returned snapshot.
	return cloneEngineInfo(info), nil
}

// createdOwnershipFailure keeps the post-create diagnostics specific to the
// operation while the matching rule stays in domain.VerifyOwnership.
func createdOwnershipFailure(failure domain.OwnershipFailure, immutableID bool) string {
	switch failure {
	case domain.OwnershipFailureSession:
		return "session ownership label does not match"
	case domain.OwnershipFailureCreation:
		return "creation generation does not match"
	case domain.OwnershipFailureReuse:
		return "reuse ownership label is missing"
	case domain.OwnershipFailureReuseGroup:
		return "reuse group label does not match"
	case domain.OwnershipFailureIdentity:
		if immutableID {
			return "immutable ID does not match"
		}
		return "unexpected immutable ID"
	default:
		return "managed ownership label is missing"
	}
}

// rollback removes a container Run created but cannot return. A failed removal
// is not hidden: without an immutable ID, Terminate refuses to delete when it
// cannot verify the generation, and the caller must know the container was left
// behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	c.stopAttached()
	if keepContainers() {
		return cause
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if c.reused {
		cfg := &config{
			runner:       c.runner,
			eng:          c.eng,
			name:         c.id,
			reuse:        true,
			creation:     c.creation,
			queryTimeout: c.queryTimeout,
			probeTimeout: c.probeTimeout,
		}
		deleted, err := rollbackReuse(cleanupCtx, c, cfg)
		if err != nil {
			return withCleanupError(cause, &CleanupError{Container: c.id, Err: err})
		}
		if !deleted {
			return withCleanupError(cause, &CleanupError{Container: c.id, Err: fmt.Errorf("stopped generation was not deleted")})
		}
		return cause
	}
	if err := c.Terminate(cleanupCtx); err != nil {
		// %v would flatten the cleanup failure into text, leaving only the
		// original recoverable through errors.Is. Join it instead.
		return withCleanupError(cause, &CleanupError{Container: c.id, Err: err})
	}
	return cause
}

// InspectInfo represents a point-in-time snapshot of container state and metadata.
type InspectInfo struct {
	id               string
	name             string
	uid              string
	image            string
	imageID          string
	imageDigest      string
	health           string
	healthStreak     int
	state            State
	exitCode         int
	exitCodeReported bool
	createdAt        time.Time
	startedAt        time.Time
	finishedAt       time.Time
	labels           map[string]string
	ip               string
	platform         string
	networkMode      string
	networks         []string
	networkAliases   map[string][]string
	ports            []PortBinding
}

// ID returns the logical container name, which is the handle's address and
// not necessarily the backend's immutable identity. ContainerID reports that
// identity separately when the backend exposes one.
func (i *InspectInfo) ID() string { return i.id }

// Name returns the backend-reported container name without Docker's leading
// slash. Apple Container addresses containers by name, so Name matches ID.
func (i *InspectInfo) Name() string { return i.name }

// ContainerID returns the backend's immutable container ID, such as Docker's
// 64-character hexadecimal ID. It is empty when the backend has no identity
// distinct from the logical name, including Apple Container. ID keeps
// returning the logical name so existing callers are unaffected.
func (i *InspectInfo) ContainerID() string { return i.uid }

func (i *InspectInfo) Image() string { return i.image }

func (i *InspectInfo) ImageID() string { return i.imageID }

func (i *InspectInfo) State() State { return i.state }

// ExitCode returns the backend value, or zero when no value was reported.
// Check State and ExitCodeReported before interpreting zero as process success.
func (i *InspectInfo) ExitCode() int { return i.exitCode }

// ExitCodeReported distinguishes a reported process result from an omitted value.
func (i *InspectInfo) ExitCodeReported() bool { return i.exitCodeReported }

// CreatedAt returns the backend-reported creation time. The zero time is the
// documented unknown marker: the backend omitted the field or reported a form
// this library cannot parse, and callers must not read it as a real instant.
// StartedAt and FinishedAt use the same marker.
func (i *InspectInfo) CreatedAt() time.Time { return i.createdAt }

func (i *InspectInfo) StartedAt() time.Time { return i.startedAt }

func (i *InspectInfo) FinishedAt() time.Time { return i.finishedAt }

func (i *InspectInfo) IP() string { return i.ip }

func (i *InspectInfo) Platform() string { return i.platform }

// ImageDigest returns the OCI root/index digest for the container image when
// the backend exposes one. Docker reports it only when the image reference used
// for the container is digest-pinned; a tag-based container returns an empty
// value even though inspect carries the resolved image ID. Apple Container
// returns the descriptor digest reported by its inspect output. The value is
// empty when the backend does not expose a digest.
func (i *InspectInfo) ImageDigest() string { return i.imageDigest }

// Health returns the backend's healthcheck status and its consecutive failed
// check count. Docker reports "starting", "healthy", or "unhealthy" and the
// streak; Apple Container has no healthcheck, so both values are empty and
// zero. An empty status means the image defines no healthcheck or the backend
// does not report one.
func (i *InspectInfo) Health() (status string, failingStreak int) {
	return i.health, i.healthStreak
}

// NetworkMode returns the backend's reported network mode. Docker reports
// "bridge", "host", "none", or a user-defined network name. Apple Container
// has no equivalent mode and returns an empty value.
func (i *InspectInfo) NetworkMode() string { return i.networkMode }

// Networks returns the names of the networks attached to the container.
// Docker reports the names from its network settings; Apple Container reports
// the networks from its inspect status. The result is a copy the caller may
// modify.
func (i *InspectInfo) Networks() []string {
	if i.networks == nil {
		return nil
	}
	clone := make([]string, len(i.networks))
	copy(clone, i.networks)
	return clone
}

// NetworkAliases returns the network-scoped DNS aliases and DNS names Docker
// reports, keyed by network name. It is nil when the backend reports none or
// does not support aliases, which is the case for Apple Container. The
// returned map and its slices are copies the caller may modify.
func (i *InspectInfo) NetworkAliases() map[string][]string {
	if i.networkAliases == nil {
		return nil
	}
	clone := make(map[string][]string, len(i.networkAliases))
	for name, aliases := range i.networkAliases {
		clone[name] = append([]string(nil), aliases...)
	}
	return clone
}

func (i *InspectInfo) Labels() map[string]string {
	if i.labels == nil {
		return nil
	}
	clone := make(map[string]string, len(i.labels))
	for k, v := range i.labels {
		clone[k] = v
	}
	return clone
}

func (i *InspectInfo) Ports() []PortBinding {
	if i.ports == nil {
		return nil
	}
	clone := make([]PortBinding, len(i.ports))
	copy(clone, i.ports)
	return clone
}

// Inspect returns a fresh snapshot of the container's metadata and runtime state.
func (c *Container) Inspect(ctx context.Context) (*InspectInfo, error) {
	info, err := c.inspectDynamicFresh(ctx)
	if err != nil {
		return nil, err
	}
	ports := make([]PortBinding, len(info.bound))
	for idx, b := range info.bound {
		ports[idx] = PortBinding{
			ContainerPort: b.containerPort,
			Proto:         b.proto,
			HostAddr:      b.hostAddr,
			HostPort:      b.hostPort,
		}
	}
	return &InspectInfo{
		id:               c.id,
		name:             info.name,
		uid:              info.uid,
		image:            info.image,
		imageID:          info.imageID,
		imageDigest:      info.imageDigest,
		health:           info.health,
		healthStreak:     info.healthFailingStreak,
		state:            info.state,
		exitCode:         info.exitCode,
		exitCodeReported: info.exitCodeReported,
		createdAt:        info.createdAt,
		startedAt:        info.startedAt,
		finishedAt:       info.finishedAt,
		labels:           info.labels,
		ip:               info.ip,
		platform:         info.platform,
		networkMode:      info.networkMode,
		networks:         info.networkNames,
		networkAliases:   info.networkAliasMap,
		ports:            ports,
	}, nil
}

// ExitCode returns the reported exit code of a stopped container.
// ErrContainerNotStopped indicates that the process has not stopped. Both that
// error and an omitted backend exit code match ErrExitCodeUnavailable.
// Apple CLI versions that omit status.exitCode cannot provide a process result.
func (c *Container) ExitCode(ctx context.Context) (int, error) {
	info, err := c.inspectDynamicFresh(ctx)
	if err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return 0, err
		}
		redacted := c.redactError(err)
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, exitCodeDeadlineError{redacted}
		}
		return 0, redacted
	}
	return reportedExitCode(info)
}

func cloneEngineInfo(info *engineInfo) *engineInfo {
	if info == nil {
		return nil
	}
	out := *info
	if info.labels != nil {
		out.labels = make(map[string]string, len(info.labels))
		for key, value := range info.labels {
			out.labels[key] = value
		}
	}
	if info.networkNames != nil {
		out.networkNames = append([]string(nil), info.networkNames...)
	}
	if info.networkAliases != nil {
		out.networkAliases = append([]string(nil), info.networkAliases...)
	}
	if info.networkAliasMap != nil {
		out.networkAliasMap = make(map[string][]string, len(info.networkAliasMap))
		for network, aliases := range info.networkAliasMap {
			out.networkAliasMap[network] = append([]string(nil), aliases...)
		}
	}
	if info.bound != nil {
		out.bound = append([]boundPort(nil), info.bound...)
	}
	if info.runOptions.capAdd != nil {
		out.runOptions.capAdd = append([]string(nil), info.runOptions.capAdd...)
	}
	if info.runOptions.capDrop != nil {
		out.runOptions.capDrop = append([]string(nil), info.runOptions.capDrop...)
	}
	if info.runOptions.ulimits != nil {
		out.runOptions.ulimits = append([]ulimitSpec(nil), info.runOptions.ulimits...)
	}
	if info.runOptions.extraHosts != nil {
		out.runOptions.extraHosts = append([]extraHostSpec(nil), info.runOptions.extraHosts...)
	}
	return &out
}

func (c *Container) rememberIdentity(ctx context.Context, info *engineInfo) error {
	identity := immutableInfo(info)
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return err
	}
	defer c.pack().inspectMu.Unlock()
	c.pack().mu.Lock()
	defer c.pack().mu.Unlock()
	if c.pack().info != nil && !sameImmutableInfo(c.pack().info, identity) {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	uid := c.immutableID()
	if uid != "" && uid != identity.uid {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if uid == "" && c.pack().info != nil && c.pack().info.uid != "" && c.pack().info.uid != identity.uid {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.creation != "" {
		actual := identity.labels[creationLabel]
		if actual != "" {
			// The shared rule also rejects a label that matches the handle's
			// generation but is not a well-formed generation ID. Handle
			// generations are always validated (newCreationID, or a reuse
			// label that checkReuseIdentity accepted), so this can only reject
			// a corrupted inspect response. It is an intentional tightening,
			// not a semantic change for valid backends.
			expected := domain.OwnershipExpected{Creation: c.creation, RequireCreation: true}
			if failure := domain.VerifyOwnership(expected, ownershipObserved(identity)); failure != domain.OwnershipFailureNone {
				return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
			}
		} else if uid == "" && c.pack().info != nil && c.pack().info.labels[creationLabel] != "" {
			// Once a name-only handle has observed its generation, losing
			// the label is also evidence of a replacement.
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
	}
	if c.pack().info == nil {
		c.pack().info = identity
	} else {
		c.pack().info = mergeImmutableInfo(c.pack().info, identity)
	}
	if c.image.reference == "" {
		c.image = imageFromInfo(info)
	}
	return nil
}

func (c *Container) cacheInfo(info *engineInfo) error {
	return c.rememberIdentity(context.Background(), info)
}

func immutableInfo(info *engineInfo) *engineInfo {
	if info == nil {
		return nil
	}
	labels := make(map[string]string, len(info.labels))
	for key, value := range info.labels {
		labels[key] = value
	}
	return &engineInfo{
		uid:                info.uid,
		image:              info.image,
		imageID:            info.imageID,
		imageDigest:        info.imageDigest,
		imageVariantDigest: info.imageVariantDigest,
		platform:           info.platform,
		platformMeta:       info.platformMeta,
		labels:             labels,
	}
}

func sameImmutableInfo(a, b *engineInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.uid != "" && b.uid != "" && a.uid != b.uid {
		return false
	}
	if a.image != "" && b.image != "" && a.image != b.image {
		return false
	}
	if a.imageID != "" && b.imageID != "" && a.imageID != b.imageID {
		return false
	}
	if a.imageDigest != "" && b.imageDigest != "" && !strings.EqualFold(a.imageDigest, b.imageDigest) {
		return false
	}
	if a.imageVariantDigest != "" && b.imageVariantDigest != "" && !strings.EqualFold(a.imageVariantDigest, b.imageVariantDigest) {
		return false
	}
	for key, value := range a.labels {
		if other, ok := b.labels[key]; ok && other != value {
			return false
		}
	}
	return true
}

func mergeImmutableInfo(old, current *engineInfo) *engineInfo {
	merged := immutableInfo(old)
	if merged.uid == "" {
		merged.uid = current.uid
	}
	if merged.image == "" {
		merged.image = current.image
	}
	if merged.imageID == "" {
		merged.imageID = current.imageID
	}
	if merged.imageDigest == "" {
		merged.imageDigest = current.imageDigest
	}
	if merged.imageVariantDigest == "" {
		merged.imageVariantDigest = current.imageVariantDigest
	}
	if merged.platform == "" {
		merged.platform = current.platform
	}
	if !merged.platformMeta.valid && current.platformMeta.valid {
		merged.platformMeta = current.platformMeta
	}
	for key, value := range current.labels {
		if _, ok := merged.labels[key]; !ok {
			merged.labels[key] = value
		}
	}
	return merged
}

// inspectTargetLocked returns the strongest target currently bound to
// the handle. The caller must hold inspectMu. A temporary name lookup is
// the only path allowed to use a logical name for Docker.
func (c *Container) inspectTargetLocked() string {
	if uid := c.immutableID(); uid != "" {
		return uid
	}
	return c.id
}

// identitySnapshot reads the handle's backend identity and creation
// generation. Both are immutable once the handle is constructed or the UID
// has been promoted, so this does not take the inspect lock; the context
// still reports an already-finished caller immediately.
func (c *Container) identitySnapshot(ctx context.Context) (uid, creation string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	return c.immutableID(), c.creation, nil
}

// verifiedOperationTarget is the safe target for lifecycle, copy, exec,
// and log operations. In particular, a Docker handle with an empty or
// malformed UID fails before a CLI call instead of falling back to its
// name. Apple handles likewise require a valid generation before using a
// name-addressed operation. It reads only immutable handle state, so it
// never waits behind an in-flight inspect or default-network lookup; the
// caller's context still bounds the operation itself.
func (c *Container) verifiedOperationTarget(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The UID is only promoted, never rewritten, and is published under
	// uidMu. creation, id, and engine are fixed when the handle is built.
	uid := c.immutableID()
	creation := c.creation
	if requiresImmutableID(c.eng) {
		if !validImmutableID(c.eng, uid) {
			return "", identityError(fmt.Sprintf("%s operation has no valid immutable container ID", c.eng.name()))
		}
		return uid, nil
	}
	if c.eng.capabilities().nameAddressed {
		if uid != "" {
			return "", identityError(fmt.Sprintf("%s handle unexpectedly carries a container ID", c.eng.name()))
		}
		if !validCreationID(creation) {
			return "", identityError(fmt.Sprintf("%s operation has no valid creation generation", c.eng.name()))
		}
	}
	if c.id == "" {
		return "", identityError("container has no logical name")
	}
	if uid != "" {
		return uid, nil
	}
	return c.id, nil
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer c.pack().inspectMu.Unlock()
	return c.inspectFreshLocked(ctx)
}

func (c *Container) inspectFreshLocked(ctx context.Context) (*engineInfo, error) {
	uid := c.immutableID()
	if requiresImmutableID(c.eng) {
		if uid != "" && !validImmutableID(c.eng, uid) {
			return nil, identityError(fmt.Sprintf("%s handle has an invalid immutable container ID", c.eng.name()))
		}
		if uid == "" && !c.nameInspect {
			return nil, identityError(fmt.Sprintf("refusing %s name inspect without a verified immutable ID", c.eng.name()))
		}
	}
	if !c.nameInspect && c.eng.capabilities().nameAddressed && !validCreationID(c.creation) {
		return nil, identityError(fmt.Sprintf("%s handle has no valid creation generation", c.eng.name()))
	}

	qCtx, cancel := context.WithTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target := c.inspectTargetLocked()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "inspect"), "inspect", target, c.id)
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		if errors.Is(err, errInspectTargetNotFound) {
			return nil, wrapInspectTargetNotFound(err)
		}
		return nil, err
	}
	if requiresImmutableID(c.eng) {
		if !validImmutableID(c.eng, info.uid) {
			return nil, identityError(fmt.Sprintf("%s inspect returned no valid immutable container ID", c.eng.name()))
		}
		if uid != "" && uid != info.uid {
			return nil, identityError(fmt.Sprintf("%s inspect returned a different immutable container ID", c.eng.name()))
		}
	}
	if !c.nameInspect && c.eng.capabilities().nameAddressed {
		if info.uid != "" {
			return nil, identityError(fmt.Sprintf("%s inspect unexpectedly returned a container ID", c.eng.name()))
		}
		if !validCreationID(c.creation) || !validCreationID(info.labels[creationLabel]) || info.labels[creationLabel] != c.creation {
			return nil, identityError(fmt.Sprintf("%s inspect returned a different creation generation", c.eng.name()))
		}
	}
	// A name-addressed discovery deliberately returns the inspected UID to
	// its caller as data only. It is never published into the handle UID here; the
	// caller must first validate managed/reuse/session ownership.
	info.observedAt = c.pack().currentTime()
	return info, nil
}

// addTimeout combines the operation's query budget with a caller-supplied
// timeout, saturating at the maximum Duration instead of overflowing. A
// caller can pass any non-negative Duration; an overflowing sum would make
// the context expire immediately and hide the backend's range validation.
func addTimeout(base, extra time.Duration) time.Duration {
	if extra > 0 && base > time.Duration(math.MaxInt64)-extra {
		return time.Duration(math.MaxInt64)
	}
	return base + extra
}
