package container

import (
	"context"
	"fmt"
)

// retainedFailedCreate returns a partial handle only after the same
// ownership checks used by automatic cleanup succeed. A missing or
// foreign container is never claimed by this Run.
func retainedFailedCreate(ctx context.Context, cfg *config, runErr, classified error) (*Container, error) {
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil, nil
	}
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(lookupCtx, cfg.name)
	if err != nil {
		return nil, fmt.Errorf("retained container %s: lock name: %w", cfg.name, err)
	}
	defer unlock()
	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectFreshLocked(lookupCtx)
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
	ctr.exposed = cfg.exposed
	ctr.published = cfg.published
	ctr.creation = cfg.creation
	ctr.uid = info.uid
	ctr.requestedPlatform = cfg.platform
	ctr.imageIdentity = imageFromInfo(info)
	ctr.image = ctr.imageIdentity
	ctr.info = info
	return ctr, nil
}

func rollbackResult(ctx context.Context, ctr *Container, cause error) (*Container, error) {
	err := ctr.rollback(ctx, cause)
	if keepContainers() {
		return ctr, err
	}
	return nil, err
}

func reuseFailureResult(ctr *Container, err error) (*Container, error) {
	if keepContainers() {
		return ctr, err
	}
	return nil, err
}
