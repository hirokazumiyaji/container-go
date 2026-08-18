package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

// keepContainers reports whether CONTAINERGO_KEEP=1 disables all
// automatic cleanup (for debugging).
func keepContainers() bool {
	return os.Getenv("CONTAINERGO_KEEP") == "1"
}

// TerminateContainer removes the container. It is nil-safe so it can be
// deferred before the error check on Run.
func TerminateContainer(ctr *Container) error {
	if ctr == nil || keepContainers() {
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
// session. The CLI has no label filter, so the listing is filtered
// client-side. It returns the IDs it removed.
func Prune(ctx context.Context) ([]string, error) {
	return pruneWith(ctx, &cli.ExecRunner{})
}

func pruneWith(ctx context.Context, r cli.Runner) ([]string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, "ls", "--all", "--format", "json")
	if err != nil {
		return nil, cli.Classify(ctx, r, err)
	}
	containers, err := inspect.Decode(stdout)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, c := range containers {
		if c.Configuration.Labels[managedLabel] != "true" || c.Status.State != string(StateStopped) {
			continue
		}
		dCtx, dCancel := withDefaultTimeout(ctx, queryTimeout)
		_, _, err := r.Run(dCtx, "delete", "--force", c.ID)
		dCancel()
		if err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("prune %s: %w", c.ID, err))
			continue
		}
		removed = append(removed, c.ID)
	}
	return removed, errors.Join(errs...)
}
