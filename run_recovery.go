package container

import (
	"context"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reaperBinaryFor resolves the CLI binary a container's watchdog target
// belongs to and reports whether reaper registration applies at all. Only
// real CLI-backed containers are registered, and CONTAINERGO_KEEP opts out
// of automatic removal entirely.
func reaperBinaryFor(cfg *config) (string, bool) {
	if keepContainers() {
		return "", false
	}
	er, ok := cfg.runner.(cli.ExternalRunner)
	if !ok || !er.External() {
		return "", false
	}
	bin := er.ExternalBinary()
	if bin == "" {
		bin = cfg.eng.binary()
	}
	return bin, true
}

// pendingReaper is the pre-create watchdog state of one create attempt.
type pendingReaper struct {
	binary     string
	subcommand string
	name       string
	creation   string
	active     bool
}

// registerPendingContainerReaper records the name and its ownership
// generation before the backend create runs, so a process that dies
// during create still leaves a generation-guarded watchdog target. A
// backend that addresses containers by immutable ID cannot name its
// target before the create returns it; those are registered after the
// create instead.
func registerPendingContainerReaper(cfg *config) pendingReaper {
	if usesImmutableIDs(cfg.eng) || !creationRE.MatchString(cfg.creation) {
		return pendingReaper{}
	}
	bin, ok := reaperBinaryFor(cfg)
	if !ok {
		return pendingReaper{}
	}
	subcommand := cfg.eng.reaperSubcommand()
	registerPendingWithGlobalReaper(bin, subcommand, cfg.name, cfg.creation)
	return pendingReaper{binary: bin, subcommand: subcommand, name: cfg.name, creation: cfg.creation, active: true}
}

// promoteContainerReaper completes a pending registration. A
// name-addressed target is confirmed, so the child stops treating its
// create as still settling; an immutable-ID target is registered by the ID
// the create returned.
func promoteContainerReaper(cfg *config, c *Container, pending pendingReaper) {
	bin, ok := reaperBinaryFor(cfg)
	if !ok {
		return
	}
	subcommand := cfg.eng.reaperSubcommand()
	if usesImmutableIDs(cfg.eng) {
		uid := c.immutableID()
		if !dockerIDRE.MatchString(uid) {
			return
		}
		registerWithGlobalReaper(bin, subcommand, uid, "")
		return
	}
	if pending.active {
		confirmPendingWithGlobalReaper(pending.binary, pending.subcommand, pending.name, pending.creation)
	}
}

// discardPendingContainerReaper withdraws a pending record when no
// create command was issued, so the child does not spend its retry budget
// on a generation that cannot exist.
func discardPendingContainerReaper(pending pendingReaper) {
	if !pending.active {
		return
	}
	discardPendingWithGlobalReaper(pending.binary, pending.subcommand, pending.name, pending.creation)
}

// registerContainerReaper registers a verified cleanup target with the
// watchdog before a destructive delete is attempted, so a failed removal
// still has a target that can be retried after the process exits.
func registerContainerReaper(cfg *config, c *Container) {
	bin, ok := reaperBinaryFor(cfg)
	if !ok {
		return
	}
	subcommand := cfg.eng.reaperSubcommand()
	if usesImmutableIDs(cfg.eng) {
		uid := c.immutableID()
		if !dockerIDRE.MatchString(uid) {
			return
		}
		registerWithGlobalReaper(bin, subcommand, uid, "")
		return
	}
	if !creationRE.MatchString(c.creation) {
		return
	}
	registerWithGlobalReaper(bin, subcommand, c.id, c.creation)
}

// unregisterContainerReaper withdraws a watchdog target after a guarded
// non-force delete has completed or refused. In particular, a refusal can
// mean that the generation started after the stopped-state revalidation; an
// active entry must not turn that refusal into a later force-delete when the
// parent exits.
func unregisterContainerReaper(cfg *config, c *Container) {
	bin, ok := reaperBinaryFor(cfg)
	if !ok || c == nil {
		return
	}
	subcommand := cfg.eng.reaperSubcommand()
	if usesImmutableIDs(cfg.eng) {
		uid := c.immutableID()
		if dockerIDRE.MatchString(uid) {
			discardPendingWithGlobalReaper(bin, subcommand, uid, "")
		}
		return
	}
	if nameRE.MatchString(c.id) && creationRE.MatchString(c.creation) {
		discardPendingWithGlobalReaper(bin, subcommand, c.id, c.creation)
	}
}

// recoverDockerRunOutput handles a successful or failed Docker run whose
// stdout did not contain a usable full container ID. Docker may have
// created the container before the output was truncated or malformed, so
// the only safe recovery is a name/generation ownership inspection. A
// verified UID is registered with the reaper after reuse ownership/state
// checks, the state is revalidated once more immediately before the
// delete, and the delete itself refuses to force-kill a generation that
// started in the meantime; every recovery failure is joined to the
// original run error.
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
	// A reuse generation is shared state. Refuse running, stopping,
	// unknown, or otherwise unverifiable generations before adding an
	// automatic-delete target to the reaper.
	if cfg.reuse {
		if info.state == StateRunning {
			unregisterContainerReaper(cfg, ctr)
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: running reuse generation may already be adopted; refusing automatic deletion", cfg.name))
		}
		if info.state != StateStopped && info.state != StateCreated {
			unregisterContainerReaper(cfg, ctr)
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: reuse generation is %s; refusing automatic deletion", cfg.name, info.state))
		}
	}
	if info.state == StateUnknown || info.state == "" {
		unregisterContainerReaper(cfg, ctr)
		return nil, withCleanupError(cause, fmt.Errorf("recover container %s: generation state is %s; refusing automatic deletion", cfg.name, info.state))
	}

	// A stopped or created generation is guarded: revalidate it immediately
	// before registering and deleting. A generation that starts in this
	// window must not be upgraded to a force delete merely because an older
	// watchdog entry was installed.
	guarded := cfg.reuse || info.state == StateStopped || info.state == StateCreated
	if guarded {
		var fresh *engineInfo
		var freshErr error
		if cfg.reuse {
			// Reuse is name-addressed for adoption purposes, so inspect the
			// name to detect a peer replacing the generation under us.
			fresh, freshErr = ctr.inspectTargetFreshRetry(delCtx, cfg.name)
		} else {
			// Ordinary recovery has an immutable UID; rechecking that UID
			// avoids a name lookup while preserving the ownership check.
			fresh, freshErr = ctr.inspectTargetFreshRetry(delCtx, ctr.immutableID())
		}
		if isNotFound(freshErr) {
			unregisterContainerReaper(cfg, ctr)
			return nil, cause
		}
		if freshErr != nil {
			unregisterContainerReaper(cfg, ctr)
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: revalidate cleanup: %w", cfg.name, freshErr))
		}
		if !failedCreateOwned(cfg, fresh) || !dockerIDRE.MatchString(fresh.uid) || fresh.uid != ctr.immutableID() {
			unregisterContainerReaper(cfg, ctr)
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: generation changed before cleanup", cfg.name))
		}
		if fresh.state != StateStopped && fresh.state != StateCreated {
			unregisterContainerReaper(cfg, ctr)
			return nil, withCleanupError(cause, fmt.Errorf("recover container %s: generation became %s; refusing automatic deletion", cfg.name, fresh.state))
		}
		info = fresh
		ctr.info = fresh
	}

	// Running/stopping ordinary generations are this run's own failed
	// create and are safe to force-remove. Reuse and settled generations use
	// the backend's non-forced form so a concurrent start is reported.
	args := cfg.eng.deleteArgs(ctr.immutableID())
	if guarded {
		args = stoppedDeleteArgsFor(cfg.eng, ctr.immutableID())
	}
	registerContainerReaper(cfg, ctr)
	if err := ctr.deleteWithArgs(delCtx, ctr.immutableID(), args); err != nil {
		if guarded {
			unregisterContainerReaper(cfg, ctr)
		}
		return nil, withCleanupError(cause, fmt.Errorf("recover container %s: cleanup: %w", cfg.name, err))
	}
	// A successful delete leaves the registered record in place as the
	// existing watchdog contract does; the next child revalidation observes
	// the missing generation and performs no delete. Only a guarded failure
	// above withdraws the entry, because that is the case where a later
	// force-delete would be unsafe.
	return nil, cause
}
