package container

import (
	"context"
	"errors"
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
	unlock, err := lockNameForBackend(lookupCtx, cfg.eng, cfg.name)
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
	if cfg.reuse {
		identity := imageFromInfo(info)
		if !identity.pinned {
			return nil, nil
		}
		requested := cfg.preparedImage
		original := info.image
		if !cfg.imagePrepared {
			requested = identity
		} else {
			original = requested.reference
		}
		if !requested.pinned || checkReuseOwnedIdentity(info, requested, original, cfg) != nil {
			return nil, nil
		}
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
	if !keepContainers() || ctr == nil {
		return nil, err
	}

	// A handle is a useful KEEP result only after the post-create target has
	// been inspected and bound to the expected generation (and platform).
	// In particular, a failed first inspect/platform resolution must not
	// publish a constructed handle that has never passed those checks.
	verifyCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	info, verifyErr := ctr.inspectFresh(verifyCtx)
	if verifyErr == nil {
		verifyErr = verifyContainerImageIdentity(ctr, info)
	}
	if verifyErr != nil {
		return nil, errors.Join(err, fmt.Errorf("verify retained container %s: %w", ctr.id, verifyErr))
	}
	return ctr, err
}

func reuseFailureResult(ctr *Container, err error) (*Container, error) {
	// A reusable create that failed post-create validation has not yet
	// crossed the ownership/image proof boundary required for a handoff.
	// KEEP must retain the backend object, but it must not manufacture an
	// unverified reuse handle for the caller.
	if keepContainers() && ctr != nil && !ctr.reused {
		return ctr, err
	}
	return nil, err
}
