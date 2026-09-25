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

// Prune removes stopped non-reuse containers created by this library,
// from any session. Shared reuse generations are left untouched; use
// PruneReuseGroup to remove an explicitly selected group. It returns the
// IDs it removed.
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

// pruneCandidate is the list-time identity snapshot for a name-addressed
// delete. A list response is not a lock: a replacement can occur before the
// fresh inspect, so every field that authorizes deletion is compared again
// while the per-name lock is held.
type pruneCandidate struct {
	id         string
	labels     map[string]string
	creation   string
	state      State
	managed    bool
	reuse      bool
	reuseGroup string
}

// pruneListed lists containers with listArgs, parses IDs, and removes each
// still-current candidate. Apple candidates are revalidated under their
// stable per-name lock before the name is used. errKind prefixes per-ID
// delete failures ("prune", …).
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
	if eng.name() == "docker" {
		return pruneDockerReuseGroupListed(ctx, r, eng, ids, errKind, reuseGroup)
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
		if eng.name() == "apple" {
			candidate, ok := listed[id]
			if !ok {
				errs = append(errs, fmt.Errorf("%s %s: list candidate metadata missing", errKind, id))
				continue
			}
			didRemove, deleteErr := pruneNamedCandidateWithMetadata(ctx, r, eng, candidate, errKind, reuseGroup)
			if deleteErr != nil {
				errs = append(errs, deleteErr)
				continue
			}
			if didRemove {
				removed = append(removed, id)
			}
			continue
		}

		dCtx, dCancel := withMaxTimeout(ctx, queryTimeout)
		_, _, err := r.Run(dCtx, eng.deleteArgs(id)...)
		dCancel()
		if err != nil && !isDeleteNotFound(eng, id, err) {
			errs = append(errs, fmt.Errorf("%s %s: %w", errKind, id, err))
			continue
		}
		removed = append(removed, id)
	}
	return removed, errors.Join(errs...)
}

// dockerPruneCandidate is the list-time snapshot for one immutable Docker
// target. Docker list output is deliberately ID-only; labels and lifecycle
// state are captured by an inspect addressed by that ID, then compared with
// a second inspect before the ID is deleted.
type dockerPruneCandidate struct {
	listedID   string
	uid        string
	labels     map[string]string
	creation   string
	state      State
	managed    bool
	reuse      bool
	reuseGroup string
}

func pruneDockerReuseGroupListed(ctx context.Context, r cli.Runner, eng engine, listedIDs []string, errKind, reuseGroup string) ([]string, error) {
	var removed []string
	var errs []error
	for _, listedID := range listedIDs {
		// A name returned by an older/fake list command is not a safe
		// list-time identity. Docker IDs are never reused, so refusing a
		// non-ID here closes the name-replacement window rather than
		// guessing which generation the list entry selected.
		if !dockerIDRE.MatchString(listedID) {
			errs = append(errs, fmt.Errorf("%s %s: list result is not a full immutable Docker ID", errKind, listedID))
			continue
		}

		candidate, err := inspectDockerPruneCandidate(ctx, r, eng, listedID)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: inspect list candidate: %w", errKind, listedID, err))
			continue
		}
		if !dockerPruneCandidateEligible(candidate, reuseGroup) {
			continue
		}

		fresh, err := inspectDockerPruneCandidate(ctx, r, eng, candidate.uid)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: inspect fresh candidate: %w", errKind, listedID, err))
			continue
		}
		if !dockerPruneCandidateStillCurrent(candidate, fresh, reuseGroup) {
			continue
		}
		target := fresh.uid
		if !validImmutableID(eng, target) {
			errs = append(errs, fmt.Errorf("%s %s: fresh inspect returned no valid immutable ID", errKind, listedID))
			continue
		}
		dCtx, dCancel := withMaxTimeout(ctx, queryTimeout)
		_, _, err = r.Run(dCtx, eng.deleteArgs(target)...)
		dCancel()
		if err != nil && !isDeleteNotFound(eng, target, err) {
			errs = append(errs, fmt.Errorf("%s %s: %w", errKind, listedID, err))
			continue
		}
		removed = append(removed, target)
	}
	return removed, errors.Join(errs...)
}

func inspectDockerPruneCandidate(ctx context.Context, r cli.Runner, eng engine, target string) (dockerPruneCandidate, error) {
	qCtx, cancel := withMaxTimeout(ctx, queryTimeout)
	defer cancel()
	ctr := &Container{id: target, runner: r, eng: eng, nameInspect: true}
	info, err := ctr.inspectFreshLocked(qCtx)
	if err != nil {
		return dockerPruneCandidate{}, err
	}
	if info == nil {
		return dockerPruneCandidate{}, fmt.Errorf("inspect returned no container identity")
	}
	return dockerPruneCandidate{
		listedID:   target,
		uid:        info.uid,
		labels:     info.labels,
		creation:   info.labels[creationLabel],
		state:      info.state,
		managed:    info.labels[managedLabel] == "true",
		reuse:      info.labels[reuseLabel] == "true",
		reuseGroup: info.labels[reuseGroupLabel],
	}, nil
}

func dockerPruneCandidateEligible(candidate dockerPruneCandidate, reuseGroup string) bool {
	if !dockerIDRE.MatchString(candidate.listedID) ||
		!dockerIDRE.MatchString(candidate.uid) ||
		!candidate.managed || !validCreationID(candidate.creation) {
		return false
	}
	if reuseGroup == "" {
		return candidate.state == StateStopped && unexpectedReuseMarker(candidate.labels) == ""
	}
	return candidate.reuse &&
		(candidate.state == StateRunning || candidate.state == StateStopped) &&
		candidate.reuseGroup == reuseGroup
}

func dockerPruneCandidateStillCurrent(listed, fresh dockerPruneCandidate, reuseGroup string) bool {
	if !dockerPruneCandidateEligible(fresh, reuseGroup) || listed.uid != fresh.uid {
		return false
	}
	if listed.state != fresh.state || listed.creation != fresh.creation {
		return false
	}
	for _, key := range []string{managedLabel, reuseLabel, reuseGroupLabel, sessionLabel} {
		if listed.labels[key] != fresh.labels[key] {
			return false
		}
	}
	return true
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
			managed:    labels[managedLabel] == "true",
			reuse:      labels[reuseLabel] == "true",
			reuseGroup: labels[reuseGroupLabel],
		}
	}
	return candidates, nil
}

func pruneCandidateEligible(candidate pruneCandidate, reuseGroup string) bool {
	if candidate.id == "" || !nameRE.MatchString(candidate.id) ||
		!candidate.managed || !validCreationID(candidate.creation) ||
		candidate.state == "" || candidate.state == StateUnknown {
		return false
	}
	if reuseGroup == "" {
		return candidate.state == StateStopped && unexpectedReuseMarker(candidate.labels) == ""
	}
	// Group prune is deliberately narrower than the old label-only query:
	// a group label alone does not prove that the object is a reusable
	// generation owned by this library. Only settled running/stopped
	// generations are eligible; transitional states remain untouched.
	return candidate.reuse && candidate.reuseGroup == reuseGroup &&
		(candidate.state == StateRunning || candidate.state == StateStopped)
}

func pruneCandidateStillCurrent(candidate pruneCandidate, fresh *engineInfo, reuseGroup string) bool {
	if fresh == nil || !pruneCandidateEligible(pruneCandidate{
		id:         candidate.id,
		labels:     fresh.labels,
		creation:   fresh.labels[creationLabel],
		state:      fresh.state,
		managed:    fresh.labels[managedLabel] == "true",
		reuse:      fresh.labels[reuseLabel] == "true",
		reuseGroup: fresh.labels[reuseGroupLabel],
	}, reuseGroup) {
		return false
	}
	if fresh.state != candidate.state || fresh.labels[creationLabel] != candidate.creation {
		return false
	}
	// Compare the ownership-bearing labels captured at list time. This
	// catches a replacement that happens to reuse a name or generation but
	// is no longer the managed/reuse/group object the caller selected.
	for _, key := range []string{managedLabel, reuseLabel, reuseGroupLabel, sessionLabel} {
		if candidate.labels[key] != fresh.labels[key] {
			return false
		}
	}
	return true
}

// pruneNamedCandidate retains the historical helper signature for package
// users. It performs a fresh, fail-closed verification when no list-time
// snapshot is available; the prune path uses the metadata-aware variant
// below to close the list/inspect race.
//
//nolint:unused // retained for package callers using the pre-metadata helper
func pruneNamedCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind, reuseGroup string) (bool, error) {
	if eng.name() != "apple" || !nameRE.MatchString(id) {
		return false, nil
	}
	dCtx, dCancel := withMaxTimeout(ctx, queryTimeout)
	defer dCancel()
	unlock, err := lockName(dCtx, id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, id, err)
	}
	defer unlock()
	fresh, err := (&Container{id: id, runner: r, eng: eng, nameInspect: true}).inspectFreshLocked(dCtx)
	if isNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, id, err)
	}
	if !pruneCandidateEligible(pruneCandidate{
		id:         id,
		labels:     fresh.labels,
		creation:   fresh.labels[creationLabel],
		state:      fresh.state,
		managed:    fresh.labels[managedLabel] == "true",
		reuse:      fresh.labels[reuseLabel] == "true",
		reuseGroup: fresh.labels[reuseGroupLabel],
	}, reuseGroup) {
		return false, nil
	}
	if reuseGroup != "" && fresh.labels[reuseGroupLabel] != reuseGroup {
		return false, nil
	}
	target, err := verifiedDeleteTarget(eng, fresh, id)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, id, err)
	}
	_, _, err = r.Run(dCtx, eng.deleteArgs(target)...)
	if err != nil && !isDeleteNotFound(eng, target, err) {
		return false, fmt.Errorf("%s %s: %w", errKind, id, err)
	}
	return true, nil
}

func pruneNamedCandidateWithMetadata(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string) (bool, error) {
	if eng.name() != "apple" || !pruneCandidateEligible(candidate, reuseGroup) {
		return false, nil
	}
	dCtx, dCancel := withMaxTimeout(ctx, queryTimeout)
	defer dCancel()
	unlock, err := lockName(dCtx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	ctr := &Container{id: candidate.id, runner: r, eng: eng, nameInspect: true}
	fresh, err := ctr.inspectFreshLocked(dCtx)
	if isNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) {
		return false, nil
	}
	target, err := verifiedDeleteTarget(eng, fresh, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	_, _, err = r.Run(dCtx, eng.deleteArgs(target)...)
	if err != nil && !isDeleteNotFound(eng, target, err) {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	return true, nil
}
