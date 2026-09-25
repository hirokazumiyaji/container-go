package container

import (
	"context"
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
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("retained container %s: inspect: %w", cfg.name, err)
	}
	if !failedCreateOwned(cfg, info) {
		return nil, nil
	}

	ctr.reused = cfg.reuse
	ctr.creation = cfg.creation
	ctr.uid = info.uid
	ctr.info = info
	return ctr, nil
}

// rollbackResult applies the automatic rollback policy while preserving
// a handle for callers that explicitly requested diagnostic retention.
// Without KEEP, Run retains its historical nil-handle-on-error contract.
func rollbackResult(ctx context.Context, c *Container, cause error) (*Container, error) {
	err := c.rollback(ctx, cause)
	if keepContainers() {
		return c, err
	}
	return nil, err
}
