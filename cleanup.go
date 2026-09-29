package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	backendinspect "github.com/hirokazumiyaji/container-go/internal/inspect"
)

// keepContainers reports whether CONTAINERGO_KEEP=1 disables automatic
// cleanup and enables the documented verified partial-handle policy.
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

type pruneCandidate struct {
	id         string
	labels     map[string]string
	creation   string
	state      State
	reuseGroup string
}

// pruneListed lists containers with listArgs, parses IDs, and removes each
// still-current candidate. Apple candidates are revalidated under their
// name lock; Docker IDs are immutable delete targets. errKind prefixes
// per-ID delete failures ("prune", …).
func pruneListed(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]string, error), errKind string) ([]string, error) {
	return pruneListedWithGroup(ctx, r, eng, listArgs, parse, errKind, "")
}

func pruneListedWithGroup(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]string, error), errKind, reuseGroup string) ([]string, error) {
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
	var listed map[string]pruneCandidate
	if usesNameAddressedDeletes(eng) {
		listed, err = applePruneCandidates(stdout)
		if err != nil {
			return nil, err
		}
	}

	var removed []string
	var errs []error
	for _, id := range ids {
		var didRemove bool
		switch {
		case usesImmutableIDs(eng):
			didRemove, err = deleteImmutablePruneCandidate(ctx, r, eng, id, errKind, reuseGroup)
		case usesNameAddressedDeletes(eng):
			candidate, ok := listed[id]
			if !ok {
				err = fmt.Errorf("%s %s: list candidate metadata missing", errKind, id)
			} else {
				didRemove, err = pruneNamedCandidate(ctx, r, eng, candidate, errKind, reuseGroup)
			}
		default:
			didRemove, err = deletePruneCandidate(ctx, r, eng, id, errKind, reuseGroup)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if didRemove {
			removed = append(removed, id)
		}
	}
	return removed, errors.Join(errs...)
}

func deleteImmutablePruneCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind, reuseGroup string) (bool, error) {
	if !dockerIDRE.MatchString(id) {
		return false, fmt.Errorf("%s %s: list result is not a full immutable Docker ID", errKind, id)
	}
	ctr := &Container{id: id, runner: r, eng: eng}
	fresh, err := ctr.inspectTargetFreshRetry(ctx, id)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, id, err)
	}
	if !pruneCandidateEligible(pruneCandidate{
		id:         id,
		labels:     fresh.labels,
		creation:   fresh.labels[creationLabel],
		state:      fresh.state,
		reuseGroup: fresh.labels[reuseGroupLabel],
	}, reuseGroup) {
		return false, nil
	}
	if !dockerIDRE.MatchString(fresh.uid) || fresh.uid != id {
		return false, nil
	}
	return deletePruneCandidate(ctx, r, eng, id, errKind, reuseGroup)
}

func deletePruneCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind, reuseGroup string) (bool, error) {
	dCtx, dCancel := withDefaultTimeout(ctx, queryTimeout)
	defer dCancel()
	args := eng.deleteArgs(id)
	if reuseGroup == "" {
		// Ordinary Prune is a stopped-container operation. A generation
		// that starts after revalidation must make docker rm/container
		// delete refuse rather than be force-killed by the cleanup path.
		args = stoppedDeleteArgsFor(eng, id)
	}
	_, _, err := r.Run(dCtx, args...)
	if err != nil {
		classified := cli.Classify(ctx, r, err, eng.probe())
		if isNotFound(classified) {
			return true, nil
		}
		return false, fmt.Errorf("%s %s: %w", errKind, id, classified)
	}
	return true, nil
}

// pruneNamedCandidate closes the list/inspect/delete race for name-
// addressed backends. The list-time candidate is only a snapshot: the
// live generation, labels, state, and reuse group are checked again while
// the stable per-name lock is held, immediately before deletion.
func pruneNamedCandidate(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	if !usesNameAddressedDeletes(eng) || !nameRE.MatchString(candidate.id) ||
		!pruneCandidateEligible(candidate, reuseGroup) {
		return false, nil
	}

	guardCtx, guardCancel := withDefaultTimeout(ctx, queryTimeout)
	defer guardCancel()
	unlock, err := lockName(guardCtx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	fresh, err := (&Container{id: candidate.id, runner: r, eng: eng}).inspectTargetFreshRetry(guardCtx, candidate.id)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) {
		return false, nil
	}
	// Apple has no immutable ID: the name lock and the exact generation
	// revalidation above are the delete proof.  Never substitute a
	// backend-reported UID for a name-addressed Apple operation.
	return deletePruneCandidate(guardCtx, r, eng, candidate.id, errKind, reuseGroup)
}

func applePruneCandidates(data []byte) (map[string]pruneCandidate, error) {
	containers, err := backendinspect.Decode(data)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]pruneCandidate, len(containers))
	for _, c := range containers {
		labels := c.Configuration.Labels
		candidates[c.ID] = pruneCandidate{
			id:         c.ID,
			labels:     labels,
			creation:   labels[creationLabel],
			state:      State(c.Status.State),
			reuseGroup: labels[reuseGroupLabel],
		}
	}
	return candidates, nil
}

func pruneCandidateEligible(candidate pruneCandidate, reuseGroup string) bool {
	if candidate.state == "" || candidate.state == StateUnknown || candidate.creation == "" ||
		!creationRE.MatchString(candidate.creation) || candidate.labels[managedLabel] != "true" {
		return false
	}
	if reuseGroup == "" {
		return candidate.state == StateStopped
	}
	return candidate.reuseGroup == reuseGroup
}

func pruneCandidateStillCurrent(candidate pruneCandidate, fresh *engineInfo, reuseGroup string) bool {
	if fresh == nil || !pruneCandidateEligible(pruneCandidate{
		id:         candidate.id,
		labels:     fresh.labels,
		creation:   fresh.labels[creationLabel],
		state:      fresh.state,
		reuseGroup: fresh.labels[reuseGroupLabel],
	}, reuseGroup) {
		return false
	}
	return fresh.state == candidate.state && fresh.labels[creationLabel] == candidate.creation
}
