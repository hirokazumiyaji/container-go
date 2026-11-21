package container

import (
	"context"
	"fmt"
)

// WithPostReadyHook registers fn to run after the readiness strategy reports
// the container ready (or immediately after start when no strategy is set) and
// before Run returns the handle. Hooks run in the order their options appear.
//
// An error from a hook fails Run. An ordinary newly created container is
// removed. A WithReuse hook failure discards the generation when this call
// created it; a call that attached to an existing generation leaves it in
// place because other callers may share it.
//
// The hook runs for every WithReuse caller, including calls that attach to an
// existing generation; use Created to run initialization such as schema setup
// only for the call that created the container. That initialization is not
// synchronized with attach callers: a concurrent or later attach can return
// from Run before the creator's hook finishes, and a hook failure after another
// caller attached causes that peer to observe ErrGenerationReplaced. Make
// cross-caller initialization idempotent or have peers detect completion
// through a wait strategy or application-level readiness signal.
//
// Run can return a usable handle together with an environment-file cleanup
// warning. That early return skips file copies and readiness work, so it also
// skips this hook; a caller must not rely on the hook when Run returns both a
// handle and an error.
func WithPostReadyHook(fn func(ctx context.Context, c *Container) error) Option {
	return func(c *config) error {
		if fn == nil {
			return validationErrorf("WithPostReadyHook", nil, "hook must not be nil")
		}
		c.postReadyHooks = append(c.postReadyHooks, fn)
		return nil
	}
}

// runPostReadyHooks preserves the hook order and identifies the failing hook
// without rendering the hook's own error details twice.
func runPostReadyHooks(ctx context.Context, c *Container, hooks []func(context.Context, *Container) error) error {
	for i, hook := range hooks {
		if err := hook(ctx, c); err != nil {
			return fmt.Errorf("post-ready hook %d: %w", i+1, err)
		}
	}
	return nil
}
