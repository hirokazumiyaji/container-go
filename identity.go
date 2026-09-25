package container

import (
	"context"
	"fmt"
	"time"
)

// The backend capability is intentionally consulted here rather than by
// comparing engine names.  Apple Container addresses a container by its
// name and needs the generation lock; Docker has a full immutable ID and
// must never fall back to a name for a normal operation.
func (c *Container) immutableID() string {
	c.uidMu.RLock()
	defer c.uidMu.RUnlock()
	return c.uid
}

// rememberImmutableID records the first immutable identity reported by the
// backend.  A later inspect result must not replace it: doing so would make
// an old handle silently address a replacement.
func (c *Container) rememberImmutableID(uid string) {
	if uid == "" {
		return
	}
	c.uidMu.Lock()
	if c.uid == "" {
		c.uid = uid
	}
	c.uidMu.Unlock()
}

// operationTarget is the sole target selector for public backend
// operations. Docker handles use their immutable ID; Apple handles use
// their name. It returns an empty string for an unbound Docker handle;
// callers that issue a backend operation use operationTargetChecked so
// they fail closed instead of falling back to the name.
func (c *Container) operationTarget() string {
	if usesImmutableIDs(c.eng) {
		return c.immutableID()
	}
	return c.id
}

func (c *Container) operationTargetChecked() (string, error) {
	target := c.operationTarget()
	if usesImmutableIDs(c.eng) && !dockerIDRE.MatchString(target) {
		return "", fmt.Errorf("container %s has no valid immutable Docker ID", c.id)
	}
	return target, nil
}

func (c *Container) verifiedOperationTarget() error {
	_, err := c.operationTargetChecked()
	return err
}

// inspectTargetFresh is a raw inspect of an explicitly selected target.  It
// is used by name-based reuse/cleanup lookups where the caller already owns
// the appropriate lock, and by immutable-ID operation paths.
func (c *Container) inspectTargetFresh(ctx context.Context, target string) (*engineInfo, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		return nil, err
	}
	if usesImmutableIDs(c.eng) && dockerIDRE.MatchString(target) {
		if info.uid != target {
			return nil, fmt.Errorf("%w: inspect target %s returned %s", ErrGenerationReplaced, target, info.uid)
		}
		c.rememberImmutableID(info.uid)
	}
	return info, nil
}

// inspectFresh is retained for internal name-addressed callers. A normal
// Docker handle must never use it: an unbound name inspect could observe a
// replacement and accidentally publish its UID. Reuse code uses
// inspectNamed, which explicitly opts into a name lookup.
// inspectTarget is the raw target selector used by the compatibility
// inspect path. It deliberately does not validate or publish an ID.
func (c *Container) inspectTarget() string {
	return c.operationTarget()
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	if usesImmutableIDs(c.eng) {
		uid := c.inspectTarget()
		if uid != "" {
			if !dockerIDRE.MatchString(uid) {
				return nil, fmt.Errorf("container %s has an invalid immutable ID %q", c.id, uid)
			}
			return c.inspectTargetFresh(ctx, uid)
		}
		if !c.nameInspect {
			return nil, fmt.Errorf("refusing Docker name inspect without an immutable ID for %s", c.id)
		}
	}
	return c.inspectTargetFresh(ctx, c.id)
}

var (
	// A daemon can briefly report a just-created container as missing while
	// its create/start transaction settles.  Retry only not-found results;
	// daemon-down and other failures remain terminal.
	inspectRetryAttempts = 3
	inspectRetryDelay    = 50 * time.Millisecond
)

func waitInspectRetry(ctx context.Context) error {
	timer := time.NewTimer(inspectRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Container) inspectTargetFreshRetry(ctx context.Context, target string) (*engineInfo, error) {
	var lastErr error
	for attempt := 0; attempt < inspectRetryAttempts; attempt++ {
		info, err := c.inspectTargetFresh(ctx, target)
		if err == nil || !isNotFound(err) || attempt+1 == inspectRetryAttempts {
			return info, err
		}
		lastErr = err
		if err := waitInspectRetry(ctx); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// acquireCurrentTarget verifies and, for Apple, locks the handle's current
// name generation.  The returned unlock function must be called by the
// caller after the backend operation completes.
func (c *Container) acquireCurrentTarget(ctx context.Context) (target string, info *engineInfo, unlock func(), err error) {
	if !usesNameAddressedDeletes(c.eng) {
		if err := c.verifiedOperationTarget(); err != nil {
			return "", nil, nil, err
		}
		target = c.operationTarget()
		return target, nil, func() {}, nil
	}
	if !creationRE.MatchString(c.creation) {
		return "", nil, nil, fmt.Errorf("container %s has no valid creation generation", c.id)
	}

	lockCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	unlock, err = lockName(lockCtx, c.id)
	if err != nil {
		return "", nil, nil, fmt.Errorf("lock container %s: %w", c.id, err)
	}
	info, err = c.inspectTargetFreshRetry(lockCtx, c.id)
	if err != nil {
		unlock()
		return "", nil, nil, err
	}
	if err := c.identityMatches(info); err != nil {
		unlock()
		return "", nil, nil, err
	}
	return c.id, info, unlock, nil
}

func (c *Container) withCurrentTarget(ctx context.Context, fn func(string) error) error {
	target, _, unlock, err := c.acquireCurrentTarget(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(target)
}

// acquireUnverifiedTarget is reserved for operations whose established
// contract is one CLI invocation. Apple still takes the same name lock for
// the whole operation; callers that need generation verification use
// acquireCurrentTarget.
func (c *Container) acquireUnverifiedTarget(ctx context.Context) (string, func(), error) {
	if !usesNameAddressedDeletes(c.eng) {
		if err := c.verifiedOperationTarget(); err != nil {
			return "", nil, err
		}
		return c.operationTarget(), func() {}, nil
	}
	lockCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	unlock, err := lockName(lockCtx, c.id)
	if err != nil {
		return "", nil, fmt.Errorf("lock container %s: %w", c.id, err)
	}
	return c.id, unlock, nil
}

func (c *Container) acquireHandleTarget(ctx context.Context) (string, func(), error) {
	if c.reused || (usesNameAddressedDeletes(c.eng) && creationRE.MatchString(c.creation)) {
		target, _, unlock, err := c.acquireCurrentTarget(ctx)
		return target, unlock, err
	}
	return c.acquireUnverifiedTarget(ctx)
}

func (c *Container) withHandleTarget(ctx context.Context, fn func(string) error) error {
	target, unlock, err := c.acquireHandleTarget(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(target)
}

// inspectCurrent is the identity-safe inspect used by State, endpoint
// lookup, and wait strategies.  Docker uses its immutable UID; Apple checks
// the managed marker and generation while holding the name lock.
func (c *Container) inspectCurrent(ctx context.Context) (*engineInfo, error) {
	target, info, unlock, err := c.acquireCurrentTarget(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if info != nil {
		return info, nil
	}
	return c.inspectTargetFreshRetry(ctx, target)
}

func (c *Container) identityMatches(info *engineInfo) error {
	if info == nil {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if usesImmutableIDs(c.eng) {
		uid := c.immutableID()
		if !dockerIDRE.MatchString(uid) || !dockerIDRE.MatchString(info.uid) || info.uid != uid {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		if c.creation != "" && (info.labels[managedLabel] != "true" ||
			!creationRE.MatchString(c.creation) || info.labels[creationLabel] != c.creation) {
			return fmt.Errorf("%w: %s is not a verified managed generation", ErrGenerationReplaced, c.id)
		}
	} else {
		if info.labels[managedLabel] != "true" {
			return fmt.Errorf("%w: %s is not managed", ErrGenerationReplaced, c.id)
		}
		if !creationRE.MatchString(c.creation) || info.labels[creationLabel] != c.creation {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
	}
	if c.reused && (info.labels[managedLabel] != "true" || info.labels[reuseLabel] != "true" ||
		!creationRE.MatchString(c.creation) || info.labels[creationLabel] != c.creation) {
		return fmt.Errorf("%w: %s is not a verified reuse generation", ErrGenerationReplaced, c.id)
	}
	return nil
}

// verifiedCurrentHandle is used only for CONTAINERGO_KEEP partial handles.
// It never consults the cached info mutex and never returns a handle unless
// a fresh inspect proves the same current identity (and, when requested,
// running state).
func (c *Container) verifiedCurrentHandle(ctx context.Context, requireRunning bool) bool {
	info, err := c.inspectCurrent(ctx)
	if err != nil || c.identityMatches(info) != nil {
		return false
	}
	return !requireRunning || info.state == StateRunning
}

// verifiedRetainedHandle accepts only the two states in which a failed
// create can still be a useful diagnostic handle.  Stopping, stopped, and
// unknown generations are not safe to hand back under CONTAINERGO_KEEP.
func (c *Container) verifiedRetainedHandle(ctx context.Context) bool {
	info, err := c.inspectCurrent(ctx)
	if err != nil || c.identityMatches(info) != nil {
		return false
	}
	return info.state == StateCreated || info.state == StateRunning
}
