package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// keepContainers reports whether CONTAINERGO_KEEP=1 disables all
// automatic cleanup (for debugging).
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

// Cleanup registers container removal via tb.Cleanup. It is nil-safe,
// so call it right after Run, before checking Run's error.
func Cleanup(tb testing.TB, ctr *Container) {
	tb.Helper()
	tb.Cleanup(func() {
		if err := TerminateContainer(ctr); err != nil {
			tb.Logf("container-go: cleanup %s: %v", ctr.ID(), err)
		}
	})
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
