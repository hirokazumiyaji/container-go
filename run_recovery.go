package container

import (
	"context"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// registerContainerReaper registers only real CLI-backed containers. The
// reaper is registered before a recovery delete so a failed cleanup still
// has an immutable watchdog target.
func registerContainerReaper(cfg *config, c *Container) {
	if keepContainers() {
		return
	}
	er, ok := cfg.runner.(cli.ExternalRunner)
	if !ok || !er.External() {
		return
	}
	bin := er.ExternalBinary()
	if bin == "" {
		bin = cfg.eng.binary()
	}
	if usesImmutableIDs(cfg.eng) {
		registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), c.immutableID(), "")
		return
	}
	registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), c.id, c.creation)
}

// recoverDockerRunOutput handles a successful or failed Docker run whose
// stdout did not contain a usable full container ID. Docker may have
// created the container before the output was truncated or malformed, so
// the only safe recovery is a name/generation ownership inspection. A
// verified UID is registered with the reaper after reuse ownership/state
// checks and before cleanup is attempted; every recovery failure is joined
// to the original run error.
func recoverDockerRunOutput(ctx context.Context, cfg *config, cause error) (*Container, error) {
	if cause == nil {
		cause = fmt.Errorf("run %s: backend did not return a full 64-hex container ID", cfg.name)
	}
	if cfg.eng.nameConflict(cause) {
		return nil, cause
	}

	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectTargetFreshRetry(lookupCtx, cfg.name)
	if isNotFound(err) {
		// No owned container is observable; the malformed output itself is
		// the primary failure and there is nothing safe to clean up.
		return nil, cause
	}
	if err != nil {
		return nil, withCleanupError(cause, fmt.Errorf("recover container %s: inspect: %w", cfg.name, err))
	}
	if !failedCreateOwned(cfg, info) {
		return nil, withCleanupError(cause, fmt.Errorf("recover container %s: inspected generation is not owned by this run", cfg.name))
	}
	if !dockerIDRE.MatchString(info.uid) {
		return nil, withCleanupError(cause, fmt.Errorf("recover container %s: inspect returned no full immutable Docker ID", cfg.name))
	}

	ctr.creation = cfg.creation
	ctr.reused = cfg.reuse
	ctr.rememberImmutableID(info.uid)
	ctr.info = info

	if keepContainers() {
		if ctr.verifiedRetainedHandle(context.WithoutCancel(ctx)) {
			return ctr, cause
		}
		return nil, cause
	}

	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	if cfg.reuse {
		// A reuse generation is shared state. Refuse running, stopping,
		// unknown, or otherwise unverifiable generations before adding an
		// automatic-delete target to the reaper.
		if info.state == StateRunning {
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: running reuse generation may already be adopted; refusing automatic deletion", cfg.name))
		}
		if info.state != StateStopped && info.state != StateCreated {
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: reuse generation is %s; refusing automatic deletion", cfg.name, info.state))
		}
		// A stopped reuse generation can be adopted or replaced between
		// the recovery lookup and rm. Reinspect the name and require the
		// same owned UID and a stopped/created state before registering or
		// deleting it.
		fresh, freshErr := ctr.inspectTargetFreshRetry(delCtx, cfg.name)
		if isNotFound(freshErr) {
			return nil, cause
		}
		if freshErr != nil {
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: revalidate cleanup: %w", cfg.name, freshErr))
		}
		if !failedCreateOwned(cfg, fresh) || !dockerIDRE.MatchString(fresh.uid) || fresh.uid != ctr.immutableID() {
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: generation changed before cleanup", cfg.name))
		}
		if fresh.state != StateStopped && fresh.state != StateCreated {
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: reuse generation became %s; refusing automatic deletion", cfg.name, fresh.state))
		}
	}
	// Register only after all ownership/state refusals above. If rm fails,
	// the reaper still owns the verified UID and can retry after the process
	// exits.
	registerContainerReaper(cfg, ctr)
	if err := ctr.delete(delCtx, ctr.immutableID()); err != nil {
		return nil, withCleanupError(cause, fmt.Errorf("recover container %s: cleanup: %w", cfg.name, err))
	}
	return nil, cause
}
