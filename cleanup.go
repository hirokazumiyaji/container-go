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
	tb.Helper()
	registerCleanup(tb, ctr, false)
}

// CleanupStrict is Cleanup with cleanup failures reported as test
// failures. It is nil-safe and otherwise has the same reuse and
// CONTAINERGO_KEEP behavior as Cleanup.
func CleanupStrict(tb testing.TB, ctr *Container) {
	tb.Helper()
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
	return pruneListed(ctx, r, eng, eng.listArgs(), eng.parseStoppedManaged, "prune")
}

type pruneCandidate struct {
	id         string
	labels     map[string]string
	creation   string
	state      State
	managed    bool
	reuse      bool
	reuseGroup string
}

// pruneListed lists candidates and removes each still-current candidate.
// Apple candidates carry list-time ownership/state metadata and are
// revalidated while the per-name lock is held before a name is deleted.
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
	if eng.name() == "apple" {
		listed, err = applePruneCandidates(stdout)
		if err != nil {
			return nil, err
		}
	}

	var removed []string
	var errs []error
	for _, id := range ids {
		var didRemove bool
		if eng.name() == "apple" {
			candidate, ok := listed[id]
			if !ok {
				errs = append(errs, fmt.Errorf("%s %s: list candidate metadata missing", errKind, id))
				continue
			}
			didRemove, err = pruneNamedCandidateWithMetadata(ctx, r, eng, candidate, errKind, reuseGroup)
		} else {
			didRemove, err = pruneDockerCandidate(ctx, r, eng, id, errKind, reuseGroup)
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

func pruneDockerCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind, reuseGroup string) (bool, error) {
	if !dockerIDRE.MatchString(id) {
		return false, nil
	}
	inspectContainer := &Container{id: id, uid: id, runner: r, eng: eng}
	fresh, err := inspectContainer.inspectFresh(ctx)
	if isNotFoundFor(eng, err) {
		unregisterContainerReaper(&config{runner: r, eng: eng}, "", "", id)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, id, err)
	}
	candidate := pruneCandidate{
		id:         id,
		labels:     fresh.labels,
		creation:   fresh.labels[creationLabel],
		state:      fresh.state,
		managed:    fresh.labels[managedLabel] == "true",
		reuse:      fresh.labels[reuseLabel] == "true",
		reuseGroup: fresh.labels[reuseGroupLabel],
	}
	if !pruneCandidateEligible(candidate, reuseGroup) {
		// In particular, ordinary prune never force-deletes a candidate
		// that became running after the daemon-side list.
		return false, nil
	}
	dCtx, dCancel := withDefaultTimeout(ctx, queryTimeout)
	defer dCancel()
	_, _, err = r.Run(dCtx, eng.deleteArgs(id)...)
	if err != nil && !isNotFoundFor(eng, err) {
		return false, fmt.Errorf("%s %s: %w", errKind, id, err)
	}
	unregisterContainerReaper(&config{runner: r, eng: eng, name: fresh.name, creation: candidate.creation}, fresh.name, candidate.creation, id)
	return true, nil
}

func applePruneCandidates(data []byte) (map[string]pruneCandidate, error) {
	containers, err := backendinspect.Decode(data)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]pruneCandidate, len(containers))
	for _, c := range containers {
		labels := make(map[string]string, len(c.Configuration.Labels))
		for key, value := range c.Configuration.Labels {
			labels[key] = value
		}
		candidates[c.ID] = pruneCandidate{
			id:         c.ID,
			labels:     labels,
			creation:   labels[creationLabel],
			state:      State(c.Status.State),
			managed:    labels[managedLabel] == "true",
			reuse:      labels[reuseLabel] == "true",
			reuseGroup: labels[reuseGroupLabel],
		}
	}
	return candidates, nil
}

func pruneCandidateEligible(candidate pruneCandidate, reuseGroup string) bool {
	if candidate.id == "" || (!nameRE.MatchString(candidate.id) && !dockerIDRE.MatchString(candidate.id)) || !candidate.managed ||
		!creationRE.MatchString(candidate.creation) || candidate.state == "" || candidate.state == StateUnknown {
		return false
	}
	if reuseGroup == "" {
		return candidate.state == StateStopped
	}
	// A group label alone does not prove that an object is a reusable
	// generation owned by this library. Only settled running/stopped
	// generations are eligible; transitional states remain untouched.
	return candidate.reuse && candidate.reuseGroup == reuseGroup &&
		(candidate.state == StateRunning || candidate.state == StateStopped)
}

func pruneCandidateStillCurrent(candidate pruneCandidate, fresh *engineInfo, reuseGroup string) bool {
	if fresh == nil {
		return false
	}
	freshCandidate := pruneCandidate{
		id:         candidate.id,
		labels:     fresh.labels,
		creation:   fresh.labels[creationLabel],
		state:      fresh.state,
		managed:    fresh.labels[managedLabel] == "true",
		reuse:      fresh.labels[reuseLabel] == "true",
		reuseGroup: fresh.labels[reuseGroupLabel],
	}
	if !pruneCandidateEligible(freshCandidate, reuseGroup) || freshCandidate.state != candidate.state || freshCandidate.creation != candidate.creation {
		return false
	}
	// Compare ownership-bearing labels captured at list time. This catches
	// a replacement that reuses the name/generation but changes the object
	// selected by the list query.
	for _, key := range []string{managedLabel, reuseLabel, reuseGroupLabel, sessionLabel} {
		if key == sessionLabel && candidate.labels[key] == "" {
			continue
		}
		if candidate.labels[key] != fresh.labels[key] {
			return false
		}
	}
	return true
}

func pruneNamedCandidateWithMetadata(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	if eng.name() != "apple" || !pruneCandidateEligible(candidate, reuseGroup) {
		return false, nil
	}
	guardCtx, guardCancel := withDefaultTimeout(ctx, queryTimeout)
	defer guardCancel()
	unlock, err := lockName(guardCtx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	fresh, err := (&Container{id: candidate.id, runner: r, eng: eng, nameInspect: true}).inspectFreshLocked(guardCtx)
	if isNotFoundFor(eng, err) {
		unregisterContainerReaper(&config{runner: r, eng: eng, name: candidate.id, creation: candidate.creation}, candidate.id, candidate.creation, "")
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) || fresh.uid != "" {
		return false, nil
	}
	_, _, err = r.Run(guardCtx, eng.deleteArgs(candidate.id)...)
	if err != nil && !isNotFoundFor(eng, err) {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	unregisterContainerReaper(&config{runner: r, eng: eng, name: candidate.id, creation: candidate.creation}, candidate.id, candidate.creation, "")
	return true, nil
}
