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
//
// A removal failure is logged rather than reported, so an existing test that
// does not care about teardown is not turned red by an unrelated backend
// problem. Use StrictCleanup when a leftover container should fail the test.
func Cleanup(tb testing.TB, ctr *Container) {
	tb.Helper()
	tb.Cleanup(func() {
		if err := TerminateContainer(ctr); err != nil {
			tb.Logf("container-go: cleanup %s: %v", ctr.ID(), err)
		}
	})
}

// StrictCleanup registers container removal like Cleanup, but reports a
// removal failure as a test failure instead of logging it. A container that
// outlives its test is a leak, and a green run would otherwise hide it.
func StrictCleanup(tb testing.TB, ctr *Container) {
	tb.Helper()
	tb.Cleanup(func() {
		if err := TerminateContainer(ctr); err != nil {
			tb.Errorf("container-go: cleanup %s left the container behind: %v", ctr.ID(), err)
		}
	})
}

// Prune removes stopped containers created by this library, from any
// session. On Apple Container, each list candidate is re-inspected and its
// generation, managed label, and state must still match before its name is
// deleted under the per-name lock. It returns the IDs it removed.
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

// pruneListed lists candidates and removes them. Backends with immutable
// delete IDs use the list target directly. Name-addressed backends retain
// list-time identity and revalidate it under the same per-name lock.
func pruneListed(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]pruneCandidate, error), errKind, reuseGroup string) ([]string, error) {
	opCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(opCtx, listArgs...)
	if err != nil {
		return nil, cli.Classify(opCtx, r, err, eng.probe())
	}
	candidates, err := parse(stdout)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, candidate := range candidates {
		if err := opCtx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", errKind, err))
			break
		}
		var didRemove bool
		if eng.nameAddressedDeletes() {
			didRemove, err = pruneNamedCandidate(opCtx, r, eng, candidate, errKind, reuseGroup)
		} else {
			didRemove, err = deletePruneCandidate(opCtx, r, eng, candidate.id, errKind)
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
	_, _, err := r.Run(ctx, eng.deleteArgs(id)...)
	if err != nil && !isNotFound(err) {
		return false, fmt.Errorf("%s %s: %w", errKind, id, err)
	}
	return true, nil
}

// pruneNamedCandidate closes the list/inspect/delete race for Apple
// Container. The fresh inspect and delete remain in one locked section.
func pruneNamedCandidate(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	if candidate.id == "" || !nameRE.MatchString(candidate.id) {
		return false, nil
	}
	unlock, err := lockName(ctx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	fresh, err := (&Container{id: candidate.id, runner: r, eng: eng}).inspectFresh(ctx)
	if isNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) {
		return false, nil
	}
	return deletePruneCandidate(ctx, r, eng, candidate.id, errKind)
}

func pruneCandidateStillCurrent(candidate pruneCandidate, fresh *engineInfo, reuseGroup string) bool {
	if fresh == nil || !candidate.managed || !creationRE.MatchString(candidate.creation) ||
		candidate.state == "" || candidate.state == StateUnknown {
		return false
	}
	if fresh.labels[managedLabel] != "true" || fresh.labels[creationLabel] != candidate.creation ||
		fresh.state != candidate.state || fresh.labels[reuseGroupLabel] != candidate.reuseGroup {
		return false
	}
	if reuseGroup != "" {
		return candidate.reuseGroup == reuseGroup && fresh.labels[reuseGroupLabel] == reuseGroup
	}
	return true
}
