package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// keepContainers reports whether CONTAINERGO_KEEP=1 disables automatic
// cleanup for newly created containers, registered Cleanup, failed-create
// cleanup, post-create rollback, and the watchdog (for debugging).
// Explicit Terminate, Prune, PruneReuseGroup, and WithReuse's stopped-
// container replacement still delete containers.
func keepContainers() bool {
	return os.Getenv("CONTAINERGO_KEEP") == "1"
}

// TerminateContainer removes the container. It is nil-safe so it can be
// deferred before the error check on Run. Shared WithReuse handles are
// left alone; call ctr.Terminate explicitly to remove a reused container.
func TerminateContainer(ctr *Container) error {
	if ctr == nil || keepContainers() || ctr.reused {
		return nil
	}
	return ctr.Terminate(context.Background())
}

type cleanupTB interface {
	Helper()
	Cleanup(func())
	Logf(string, ...any)
	Errorf(string, ...any)
}

func registerCleanup(tb cleanupTB, ctr *Container, strict bool) {
	tb.Helper()
	tb.Cleanup(func() {
		if err := TerminateContainer(ctr); err != nil {
			if strict {
				tb.Errorf("container-go: cleanup %s left the container behind: %v", ctr.ID(), err)
				return
			}
			tb.Logf("container-go: cleanup %s: %v", ctr.ID(), err)
		}
	})
}

// Cleanup registers best-effort container removal via tb.Cleanup. It is
// nil-safe, so call it right after Run, before checking Run's error.
//
// A removal failure is logged rather than reported, so an existing test that
// does not care about teardown is not turned red by an unrelated backend
// problem. Use StrictCleanup when a leftover container should fail the test.
func Cleanup(tb testing.TB, ctr *Container) {
	registerCleanup(tb, ctr, false)
}

// StrictCleanup registers container removal like Cleanup, but reports a
// removal failure as a test failure instead of logging it. A container that
// outlives its test is a leak, and a green run would otherwise hide it.
// It is nil-safe and otherwise has the same reuse and CONTAINERGO_KEEP
// behavior as Cleanup.
func StrictCleanup(tb testing.TB, ctr *Container) {
	registerCleanup(tb, ctr, true)
}

// CleanupStrict is an alias for StrictCleanup.
func CleanupStrict(tb testing.TB, ctr *Container) {
	StrictCleanup(tb, ctr)
}

// Prune removes stopped containers created by this library, from any
// session. It returns the IDs it removed.
func Prune(ctx context.Context) ([]string, error) {
	eng, err := detectEngine()
	if err != nil {
		return nil, err
	}
	return pruneWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng)
}

func pruneWith(ctx context.Context, r cli.Runner, eng engine) ([]string, error) {
	return pruneListed(ctx, r, eng, eng.listArgs(), eng.parseStoppedManaged, "prune")
}

// pruneListed lists containers with listArgs, parses IDs, and force-deletes
// each one. errKind prefixes per-ID delete failures ("prune", …).
func pruneListed(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]string, error), errKind string) ([]string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, listArgs...)
	if err != nil {
		return nil, cli.Classify(ctx, r, err, eng.probe())
	}
	ids, err := parse(stdout)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, id := range ids {
		dCtx, dCancel := withDefaultTimeout(ctx, queryTimeout)
		_, _, err := r.Run(dCtx, eng.deleteArgs(id)...)
		dCancel()
		if err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("%s %s: %w", errKind, id, err))
			continue
		}
		removed = append(removed, id)
	}
	return removed, errors.Join(errs...)
}
