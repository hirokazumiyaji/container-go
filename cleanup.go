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
// The complete operation, including the Apple name-lock wait, is bounded
// by terminateTimeout so a stuck peer cannot stall test cleanup forever.
func TerminateContainer(ctr *Container) error {
	if ctr == nil || keepContainers() || ctr.reused {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminateTimeout)
	defer cancel()
	return ctr.Terminate(ctx)
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
// session. On Apple Container, each list candidate is re-inspected and
// its generation, managed label, and state must still match at that
// instant before the name is deleted under the stable per-name lock. It
// returns the IDs it removed. The lock coordinates library operations
// that use the guarded name-lock protocol, but a direct Apple Container
// CLI call, an unguarded operation such as Stop, or another external
// actor can mutate the name after inspect; such state changes are
// outside this guarantee.
func Prune(ctx context.Context) ([]string, error) {
	eng, err := detectEngine()
	if err != nil {
		return nil, err
	}
	return pruneWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng)
}

func pruneWith(ctx context.Context, r cli.Runner, eng engine) ([]string, error) {
	return pruneListed(ctx, r, eng, eng.listArgs(), eng.parseStoppedManaged, "prune", "")
}

// pruneListed lists candidates and force-deletes them. For a backend that
// deletes by immutable ID, the list result is already safe to target. Apple
// Container deletes by name, so each candidate is revalidated under the
// per-name lock immediately before its delete. reuseGroup is empty for
// ordinary Prune and names the required group for PruneReuseGroup.
func pruneListed(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]pruneCandidate, error), errKind, reuseGroup string) ([]string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, listArgs...)
	if err != nil {
		return nil, cli.Classify(ctx, r, err, eng.probe())
	}
	candidates, err := parse(stdout)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, candidate := range candidates {
		var didRemove bool
		if eng.nameAddressedDeletes() {
			didRemove, err = pruneNamedCandidate(ctx, r, eng, candidate, errKind, reuseGroup)
		} else {
			didRemove, err = deletePruneCandidate(ctx, r, eng, candidate.id, errKind)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if didRemove {
			removed = append(removed, candidate.id)
		}
	}
	return removed, errors.Join(errs...)
}

func deletePruneCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind string) (bool, error) {
	dCtx, dCancel := withDefaultTimeout(ctx, queryTimeout)
	defer dCancel()
	_, _, err := r.Run(dCtx, eng.deleteArgs(id)...)
	if err != nil && !isNotFound(err) {
		return false, fmt.Errorf("%s %s: %w", errKind, id, err)
	}
	return true, nil
}

// pruneNamedCandidate closes the list/inspect/delete race for Apple
// Container. A stale or foreign live container is skipped without an
// error: it is no longer the candidate the caller asked us to remove.
// Operational failures, however, are returned rather than treated as a
// successful safety check.
func pruneNamedCandidate(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	if candidate.id == "" || !nameRE.MatchString(candidate.id) {
		return false, nil
	}

	// Keep the lock, fresh inspect, and delete in one critical section.
	// The create paths in this package take the same lock, so a library
	// peer cannot recreate the name between the checks and delete.
	guardCtx, guardCancel := withDefaultTimeout(ctx, queryTimeout)
	defer guardCancel()
	unlock, err := lockName(guardCtx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	fresh, err := (&Container{id: candidate.id, runner: r, eng: eng}).inspectFresh(guardCtx)
	if isNotFound(err) {
		// It disappeared on its own; there is nothing left to delete.
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) {
		return false, nil
	}

	target := candidate.id
	if fresh.uid != "" {
		// This is normally empty for Apple, but use an immutable ID if
		// a backend supplies one rather than falling back to the name.
		target = fresh.uid
	}
	return deletePruneCandidate(guardCtx, r, eng, target, errKind)
}

func pruneCandidateStillCurrent(candidate pruneCandidate, fresh *engineInfo, reuseGroup string) bool {
	if !candidate.managed || candidate.creation == "" || candidate.state == "" || candidate.state == StateUnknown {
		return false
	}
	if fresh.labels[managedLabel] != "true" {
		return false
	}
	if fresh.labels[creationLabel] == "" || fresh.labels[creationLabel] != candidate.creation {
		return false
	}
	if fresh.state != candidate.state {
		return false
	}
	if reuseGroup != "" {
		return candidate.reuseGroup == reuseGroup && fresh.labels[reuseGroupLabel] == reuseGroup
	}
	return true
}
