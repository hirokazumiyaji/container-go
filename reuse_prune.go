package container

import (
	"context"
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

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
