package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// deleteStoppedReuse removes a stopped reuse container only after a
// fresh ownership/state check. Docker binds the delete to the original
// immutable UID; Apple holds the cooperating-process name lock across
// the fresh inspect and delete. A same-generation running replacement is
// therefore never deleted.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	deleted, err := deleteStoppedReuseResult(ctx, cfg, info)
	if err != nil {
		return err
	}
	if !deleted {
		return nil
	}
	return nil
}

func deleteStoppedReuseResult(ctx context.Context, cfg *config, info *engineInfo) (bool, error) {
	if info == nil {
		return false, fmt.Errorf("reuse %s: missing inspected container", cfg.name)
	}
	// A container carrying neither ownership marker is not a reuse
	// generation, so it is left untouched; pre-library Docker containers
	// can otherwise look like a stopped reuse result. Once either marker
	// is present the object claims ownership of this namespace, and a
	// partial claim fails closed instead of being silently skipped. This
	// helper is reached only after checkReuseOwnedIdentity or a create this
	// library owns, so skipping a markerless container changes no production
	// deletion path.
	if info.labels[managedLabel] != "true" && info.labels[reuseLabel] != "true" {
		return false, nil
	}
	expected := domain.OwnershipExpected{
		RequireManaged:  true,
		RequireReuse:    true,
		RequireCreation: true,
	}
	switch domain.VerifyOwnership(expected, ownershipObserved(info)) {
	case domain.OwnershipFailureNone:
	case domain.OwnershipFailureManaged:
		return false, fmt.Errorf("reuse %s: managed ownership label is missing", cfg.name)
	case domain.OwnershipFailureCreation:
		return false, fmt.Errorf("reuse %s: creation ownership label is missing or invalid", cfg.name)
	default:
		return false, nil
	}
	creation := info.labels[creationLabel]
	if info.state != StateStopped {
		return false, nil
	}
	if cfg.eng.capabilities().immutableID && !validImmutableID(cfg.eng, info.uid) {
		return false, fmt.Errorf("reuse %s: stopped container has no verified immutable ID", cfg.name)
	}

	target := cfg.name
	guardCtx, cancel := withDefaultTimeout(ctx, cfg.effectiveQueryTimeout())
	defer cancel()
	unlock := func() {}
	if !requiresImmutableID(cfg.eng) {
		var err error
		unlock, err = lockName(guardCtx, cfg.name)
		if err != nil {
			return false, fmt.Errorf("reuse %s: lock name: %w", cfg.name, err)
		}
	}
	defer unlock()

	freshContainer := namedContainer(cfg, cfg.name)
	fresh, err := freshContainer.inspectFreshLocked(guardCtx)
	if isNotFoundFor(cfg.eng, err) {
		unregisterContainerReaper(cfg, cfg.name, creation, target)
		return false, nil
	}
	if err != nil {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: verify stopped generation: %w", cfg.name, err)
	}
	if !sameStoppedReuseGeneration(info, fresh) {
		unregisterContainerReaper(cfg, cfg.name, creation, info.uid)
		return false, nil
	}
	if fresh.state != StateStopped {
		protectReuseReaper(cfg)
		return false, nil
	}
	if cfg.eng.capabilities().immutableID {
		if !validImmutableID(cfg.eng, fresh.uid) || fresh.uid != info.uid {
			unregisterContainerReaper(cfg, cfg.name, creation, info.uid)
			return false, nil
		}
		target = info.uid
	} else if fresh.uid != "" {
		unregisterContainerReaper(cfg, cfg.name, creation, "")
		return false, fmt.Errorf("reuse %s: %s inspect returned an unexpected immutable ID", cfg.name, cfg.eng.name())
	}
	_, _, err = cfg.runner.Run(guardCtx, cfg.eng.deleteArgs(target)...)
	if err != nil && !isNotFoundFor(cfg.eng, err) {
		return false, fmt.Errorf("reuse %s: delete stopped generation: %w", cfg.name, err)
	}
	unregisterContainerReaper(cfg, cfg.name, creation, target)
	return true, nil
}

func sameStoppedReuseGeneration(before, after *engineInfo) bool {
	if before == nil || after == nil {
		return false
	}
	return after.state == StateStopped && domain.SameOwnershipLabels(before.labels, after.labels)
}

func rollbackReuse(ctx context.Context, ctr *Container, cfg *config) (bool, error) {
	if ctr == nil {
		return false, errors.New("reuse: missing container for rollback")
	}
	info := ctr.infoSnapshot()
	if info == nil {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: missing inspected generation for rollback", cfg.name)
	}
	if info.state != StateStopped {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: %s generation may be shared; refusing automatic deletion", cfg.name, info.state)
	}
	deleted, err := deleteStoppedReuseResult(ctx, cfg, info)
	if err != nil {
		protectReuseReaper(cfg)
		return false, err
	}
	if !deleted {
		protectReuseReaper(cfg)
		return false, fmt.Errorf("reuse %s: stopped generation changed; refusing automatic deletion", cfg.name)
	}
	return true, nil
}

func inspectRetryError(ctx context.Context, err error) error {
	if retryableInspectError(ctx, err) {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func retryableInspectError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, ErrSystemNotRunning) || errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidConfig) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return !cli.IsProbeConfigurationError(err)
	}
	return transientReuseInspectError(err)
}

func transientReuseInspectError(err error) bool {
	if err == nil || errors.Is(err, ErrSystemNotRunning) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	message := err.Error()
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		message = cliErr.Stderr
	}
	message = strings.ToLower(message)
	for _, marker := range []string{
		"temporary", "temporarily", "try again", "timeout", "timed out",
		"temporarily unavailable", "unavailable", "connection", "transport",
		"xpc", "busy", "not ready", "try later", "eof", "reset by peer",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func reuseInspectWaitError(name string, waitErr, lastInspectErr error) error {
	if waitErr == nil {
		return nil
	}
	if lastInspectErr == nil {
		if errors.Is(waitErr, context.DeadlineExceeded) {
			return fmt.Errorf("reuse %s: timed out waiting for a usable container: %w", name, waitErr)
		}
		return waitErr
	}
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container (last inspect error: %w): %w", name, lastInspectErr, waitErr)
	}
	return fmt.Errorf("reuse %s: %w (last inspect error: %w)", name, waitErr, lastInspectErr)
}

// PruneReuseGroup force-removes every container tagged with the given
// WithReuseGroup value, running or stopped. Use it as a CI teardown
// step; ordinary Prune still only removes stopped managed containers.
// PruneOption values such as WithPruneOlderThan, WithPruneLabel, and
// WithPruneDryRun apply.
func PruneReuseGroup(ctx context.Context, group string, opts ...PruneOption) ([]string, error) {
	if err := validateReuseGroupFor("PruneReuseGroup", group); err != nil {
		return nil, err
	}
	cfg, err := parsePruneOptions("PruneReuseGroup", opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	eng, err := detectEngine()
	if err != nil {
		return nil, safePublicError(err)
	}
	overallCtx, overallCancel := context.WithTimeout(ctx, cfg.timeout)
	defer overallCancel()
	runner, eng, err := cfg.prepareTarget(overallCtx, eng, group)
	if err != nil {
		return nil, err
	}
	return pruneReuseGroupWithConfig(overallCtx, runner, eng, group, cfg)
}

func pruneReuseGroupWith(ctx context.Context, r cli.Runner, eng engine, group string, opts ...PruneOption) ([]string, error) {
	if err := validateReuseGroupFor("PruneReuseGroup", group); err != nil {
		return nil, safePublicError(err, group)
	}
	cfg, err := parsePruneOptions("PruneReuseGroup", opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneReuseGroupWithConfig(ctx, r, eng, group, cfg)
}

func pruneReuseGroupWithConfig(ctx context.Context, r cli.Runner, eng engine, group string, cfg pruneConfig) ([]string, error) {
	if err := validateReuseGroupFor("PruneReuseGroup", group); err != nil {
		return nil, safePublicError(err, group)
	}
	return pruneListedWithGroupConfig(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, map[string]pruneCandidate, error) {
		return eng.parsePruneListed(data, group)
	}, "prune reuse group "+group, group, cfg)
}

func reuseContextError(ctx context.Context, name string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reuse %s: timed out waiting for a usable container: %w", name, context.DeadlineExceeded)
	}
	return ctx.Err()
}
