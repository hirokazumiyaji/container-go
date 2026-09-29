package container

import (
	"context"
	"errors"
	"fmt"
)

// failedCreateOwned reports whether an inspected container is tied to
// this exact failed create. Missing ownership metadata is deliberately
// treated as unowned: automatic deletion must fail closed.
func failedCreateOwned(cfg *config, info *engineInfo) bool {
	if info == nil || info.labels[managedLabel] != "true" ||
		info.labels[sessionLabel] != sessionID() {
		return false
	}
	if !creationRE.MatchString(cfg.creation) {
		return false
	}
	actual, ok := info.labels[creationLabel]
	if !ok || actual != cfg.creation {
		return false
	}
	if cfg.reuse && info.labels[reuseLabel] != "true" {
		return false
	}
	return true
}

// retainedFailedCreate looks up a container left by a failed create when
// diagnostic retention is enabled. It returns a usable handle only after
// the same ownership checks used by automatic cleanup succeed. A missing
// or foreign container is not claimed by this Run. This lookup is also
// used for name-conflict and create-race errors, because the container
// may have been created before the CLI reported one of those failures.
func retainedFailedCreate(ctx context.Context, cfg *config, _ error, _ error) (*Container, error) {
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(lookupCtx, cfg.name)
	if err != nil {
		return nil, fmt.Errorf("retained container %s: lock name: %w", cfg.name, err)
	}
	defer unlock()

	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectFresh(lookupCtx)
	if err != nil {
		if isNotFoundFor(cfg.eng, cfg.name, err) {
			return nil, nil
		}
		return nil, fmt.Errorf("retained container %s: inspect: %w", cfg.name, err)
	}
	if !failedCreateOwned(cfg, info) {
		return nil, nil
	}

	ctr.reused = cfg.reuse
	ctr.creation = cfg.creation
	ctr.setImmutableID(info.uid)
	ctr.info = info
	return ctr, nil
}

// rollbackResult applies the automatic rollback policy while preserving
// a handle for callers that explicitly requested diagnostic retention.
// Without KEEP, Run retains its historical nil-handle-on-error contract.
// With KEEP the handle is revalidated first: a handle that can no longer
// be bound to the generation this Run created is never published, and
// the verification failure is joined with the original cause.
func rollbackResult(ctx context.Context, c *Container, cause error) (*Container, error) {
	err := c.rollback(ctx, cause)
	if keepContainers() {
		verified, verifyErr := verifyRetainedHandle(ctx, c)
		if verifyErr != nil {
			return nil, withCleanupError(cause, verifyErr)
		}
		return verified, cause
	}
	return nil, err
}

// verifyRetainedHandle re-inspects a container this Run created and
// returns a handle bound to the observed generation. Labels, session and
// generation must still match the handle: a replaced or foreign
// same-name container must not be published as this Run's partial
// handle.
func verifyRetainedHandle(ctx context.Context, c *Container) (*Container, error) {
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(lookupCtx, c.id)
	if err != nil {
		return nil, fmt.Errorf("retained container %s: lock name: %w", c.id, err)
	}
	defer unlock()

	ctr := &Container{
		id:        c.id,
		runner:    c.runner,
		eng:       c.eng,
		exposed:   c.exposed,
		published: c.published,
		reused:    c.reused,
		creation:  c.creation,
		uid:       c.immutableID(),
	}
	info, err := ctr.inspectFresh(lookupCtx)
	if err != nil {
		return nil, fmt.Errorf("retained container %s: inspect: %w", c.id, err)
	}
	if err := verifyRetainedIdentity(c, info); err != nil {
		return nil, fmt.Errorf("retained container %s: %w", c.id, err)
	}
	ctr.creation = info.labels[creationLabel]
	ctr.setImmutableID(info.uid)
	ctr.info = info
	return ctr, nil
}

// verifyRetainedIdentity reports whether info still describes the exact
// generation the handle was created for. A reuse handle only needs the
// shared ownership labels: a generation created by another process is a
// legitimate container for that name.
func verifyRetainedIdentity(c *Container, info *engineInfo) error {
	if info == nil {
		return errors.New("inspect returned no container")
	}
	if info.labels[managedLabel] != "true" {
		return errors.New("container is no longer managed by container-go")
	}
	if actual, ok := info.labels[creationLabel]; !ok || actual != c.creation {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.reused {
		if info.labels[reuseLabel] != "true" {
			return errors.New("container is no longer marked for reuse")
		}
		return nil
	}
	if info.labels[sessionLabel] != sessionID() {
		return errors.New("container is no longer owned by this process")
	}
	return nil
}
