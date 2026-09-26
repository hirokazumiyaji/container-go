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
	ctx, cancel := withDefaultTimeout(context.Background(), terminateTimeout)
	defer cancel()
	return ctr.Terminate(ctx)
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
				tb.Errorf("container-go: cleanup %s: %v", ctr.ID(), err)
				return
			}
			tb.Logf("container-go: cleanup %s: %v", ctr.ID(), err)
		}
	})
}

// Cleanup registers best-effort container removal via tb.Cleanup. It is
// nil-safe, so call it right after Run, before checking Run's error.
func Cleanup(tb testing.TB, ctr *Container) {
	registerCleanup(tb, ctr, false)
}

// CleanupStrict is Cleanup with cleanup failures reported as test
// failures. It is nil-safe and otherwise has the same reuse and
// CONTAINERGO_KEEP behavior as Cleanup.
func CleanupStrict(tb testing.TB, ctr *Container) {
	registerCleanup(tb, ctr, true)
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
	return pruneListed(ctx, r, eng, eng.listArgs(), eng.parseStoppedManaged, "prune", "")
}

// pruneListed lists candidates and removes them. Name-addressed engines
// revalidate the list-time generation, ownership, and state while holding
// the stable name lock across inspect and delete.
func pruneListed(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]pruneCandidate, error), errKind, reuseGroup string) ([]string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, listArgs...)
	if err != nil {
		return nil, cli.Classify(qCtx, r, err, eng.probe())
	}
	candidates, err := parse(stdout)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, candidate := range candidates {
		// Gate on the caller's cancellation, not the list budget: every
		// candidate gets its own queryTimeout so one slow delete cannot
		// exhaust the remaining candidates' allowance.
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", errKind, err))
			break
		}
		cCtx, cCancel := withDefaultTimeout(ctx, queryTimeout)
		var didRemove bool
		if usesNameAddressedDeletes(eng) {
			didRemove, err = pruneNamedCandidate(cCtx, r, eng, candidate, errKind, reuseGroup)
		} else {
			didRemove, err = pruneImmutableCandidate(cCtx, r, eng, candidate, errKind, reuseGroup)
		}
		cCancel()
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

// pruneImmutableCandidate re-verifies an immutable-ID candidate before
// deleting it. The name-addressed path already re-inspects, and without the
// same check a container that restarted between the list call and the delete
// would be force-removed even though Prune only targets stopped containers.
//
// The list call yields only IDs on this backend, so ownership is derived
// from the fresh inspect rather than compared against the list.
func pruneImmutableCandidate(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	stdout, _, err := r.Run(ctx, eng.inspectArgs(candidate.id)...)
	if err != nil {
		if isNotFoundFor(eng, err) {
			// Already gone: an idempotent success that reports nothing
			// removed, because this call did not remove it.
			return true, nil
		}
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	fresh, err := eng.parseInspect(stdout, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if fresh.uid != candidate.id {
		return false, fmt.Errorf("%s %s: inspect returned a different immutable ID", errKind, candidate.id)
	}
	if reuseGroup != "" {
		if fresh.labels[reuseGroupLabel] != reuseGroup {
			return false, nil
		}
	} else if fresh.labels[managedLabel] != "true" || fresh.state != StateStopped {
		// A plain Prune only removes stopped managed containers, so a
		// container that restarted since the list call is left alone.
		return false, nil
	}
	if err := verifyDestructiveInfo(eng, fresh, false, reuseGroup == ""); err != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	return deletePruneCandidate(ctx, r, eng, candidate.id, errKind)
}

func deletePruneCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if requiresImmutableID(eng) && !dockerIDRE.MatchString(id) {
		return false, fmt.Errorf("%s %s: Docker prune target is not a full immutable ID", errKind, id)
	}
	if !requiresImmutableID(eng) && !usesNameAddressedDeletes(eng) {
		return false, fmt.Errorf("%s %s: backend has no safe prune target", errKind, id)
	}
	if !requiresImmutableID(eng) && !nameRE.MatchString(id) {
		return false, fmt.Errorf("%s %s: invalid container name", errKind, id)
	}
	_, _, err := r.Run(ctx, eng.deleteArgs(id)...)
	if err != nil {
		classified := wrapNotFoundFor(eng, cli.Classify(ctx, r, err, eng.probe()))
		if !isNotFoundFor(eng, classified) {
			return false, fmt.Errorf("%s %s: %w", errKind, id, classified)
		}
	}
	return true, nil
}

func pruneNamedCandidate(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	if candidate.id == "" || !nameRE.MatchString(candidate.id) {
		return false, nil
	}
	guardCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	unlock, err := lockName(guardCtx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	fresh, err := (&Container{id: candidate.id, runner: r, eng: eng}).inspectFresh(guardCtx)
	if isNotFoundFor(eng, err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) {
		return false, nil
	}
	// A reuse-group prune force-removes every container tagged with the
	// group, so the managed label must not gate it. A plain Prune keeps the
	// requirement.
	if err := verifyDestructiveInfo(eng, fresh, false, reuseGroup == ""); err != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	target, err := verifiedDeleteTarget(eng, fresh, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	return deletePruneCandidate(guardCtx, r, eng, target, errKind)
}

// pruneCandidateStillCurrent reports whether a fresh inspect still proves
// the listed candidate is the same container, so a delete cannot land on a
// replacement.
//
// A reuse-group prune is keyed on group membership alone: PruneReuseGroup
// force-removes every container tagged with the group, whether or not it
// carries the managed label. Requiring `managed` there would silently skip
// group members and still report success.
func pruneCandidateStillCurrent(candidate pruneCandidate, fresh *engineInfo, reuseGroup string) bool {
	if fresh == nil || !validCreationID(candidate.creation) || !knownPruneState(candidate.state) {
		return false
	}
	if !validCreationID(fresh.labels[creationLabel]) ||
		fresh.labels[creationLabel] != candidate.creation || fresh.state != candidate.state ||
		fresh.labels[reuseGroupLabel] != candidate.reuseGroup {
		return false
	}
	if reuseGroup != "" {
		return candidate.reuseGroup == reuseGroup && fresh.labels[reuseGroupLabel] == reuseGroup && knownPruneState(fresh.state)
	}
	return candidate.managed && fresh.labels[managedLabel] == "true" && fresh.state == StateStopped
}

func knownPruneState(state State) bool {
	return knownStableState(state)
}
