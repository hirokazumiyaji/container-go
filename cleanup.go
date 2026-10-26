package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// Cleanup registers container removal via tb.Cleanup. It is nil-safe,
// so call it right after Run, before checking Run's error.
//
// A removal failure is logged rather than reported, so an existing test that
// does not care about teardown is not turned red by an unrelated backend
// problem. Use StrictCleanup when a leftover container should fail the test.
func Cleanup(tb testing.TB, ctr *Container) {
	tb.Helper()
	registerCleanup(tb, ctr, false)
}

// StrictCleanup registers container removal like Cleanup, but reports a
// removal failure as a test failure instead of logging it. A container that
// outlives its test is a leak, and a green run would otherwise hide it.
// It is nil-safe and otherwise has the same reuse and CONTAINERGO_KEEP
// behavior as Cleanup.
func StrictCleanup(tb testing.TB, ctr *Container) {
	tb.Helper()
	registerCleanup(tb, ctr, true)
}

// PruneOption configures Prune and PruneReuseGroup operations.
type PruneOption func(*pruneConfig) error

type pruneConfig struct {
	timeout     time.Duration
	parallelism int
	runner      Runner
	binary      string
	olderThan   time.Duration
	now         func() time.Time
}

const defaultPruneParallelism = 4

var defaultPruneTimeout = 5 * time.Minute

// WithPruneTimeout sets an overall timeout for the prune operation.
// When omitted, the operation uses defaultPruneTimeout (5 minutes)
// unless the caller's context already has an earlier deadline.
func WithPruneTimeout(d time.Duration) PruneOption {
	return func(c *pruneConfig) error {
		if d <= 0 {
			return validationErrorf("WithPruneTimeout", d, "timeout must be positive")
		}
		c.timeout = d
		return nil
	}
}

// WithPruneOlderThan limits Prune and PruneReuseGroup to candidates the
// backend reports as created before now-d. A non-positive duration is
// rejected. Candidates whose creation time is unknown are retained rather
// than treated as old enough. Ordinary Prune still removes only stopped
// managed containers, so a running leftover remains in place.
func WithPruneOlderThan(d time.Duration) PruneOption {
	return func(c *pruneConfig) error {
		if d <= 0 {
			return validationErrorf("WithPruneOlderThan", d, "age must be positive")
		}
		c.olderThan = d
		return nil
	}
}

// WithPruneParallelism sets the worker pool size for concurrent container
// deletions during prune. When omitted, defaultPruneParallelism (4) is used;
// a non-positive value is rejected.
func WithPruneParallelism(n int) PruneOption {
	return func(c *pruneConfig) error {
		if n <= 0 {
			return validationErrorf("WithPruneParallelism", n, "parallelism must be positive")
		}
		c.parallelism = n
		return nil
	}
}

// WithPruneRunner sets the runner used for Prune or PruneReuseGroup.
func WithPruneRunner(r Runner) PruneOption {
	return func(c *pruneConfig) error {
		if runnerIsNil(r) {
			return validationErrorf("WithPruneRunner", nil, "runner must not be nil")
		}
		c.runner = r
		applyBinaryToRunner(c.runner, c.binary)
		return nil
	}
}

// WithPruneBinary sets the executable name or path for the container CLI
// during Prune or PruneReuseGroup.
func WithPruneBinary(path string) PruneOption {
	return func(c *pruneConfig) error {
		if path == "" {
			return validationErrorf("WithPruneBinary", path, "binary path must not be empty")
		}
		if strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
			return validationErrorf("WithPruneBinary", path, "invalid binary path %q", path)
		}
		c.binary = path
		applyBinaryToRunner(c.runner, path)
		return nil
	}
}

// runnerFor returns the runner these options select, falling back to a new
// ExecRunner for the engine's default CLI and target environment.
func (c pruneConfig) runnerFor(eng engine) Runner {
	return effectiveRunner(c.runner, c.binary, eng)
}

// parsePruneOptions applies opts over the prune defaults. name labels the
// entry point for a nil option, matching Run and Exec.
func parsePruneOptions(name string, opts []PruneOption) (pruneConfig, error) {
	cfg := pruneConfig{
		timeout:     defaultPruneTimeout,
		parallelism: defaultPruneParallelism,
		now:         time.Now,
	}
	for i, opt := range opts {
		if opt == nil {
			return pruneConfig{}, validationErrorf(name, i, "option %d is nil", i)
		}
		if err := opt(&cfg); err != nil {
			return pruneConfig{}, err
		}
	}
	return cfg, nil
}

// Prune removes stopped containers created by this library, from any
// session. PruneOption values such as WithPruneTimeout and
// WithPruneOlderThan apply. It returns the IDs it removed.
func Prune(ctx context.Context, opts ...PruneOption) ([]string, error) {
	cfg, err := parsePruneOptions("Prune", opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	eng, err := detectEngine()
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneWithConfig(ctx, cfg.runnerFor(eng), eng, cfg)
}

func pruneWith(ctx context.Context, r cli.Runner, eng engine, opts ...PruneOption) ([]string, error) {
	cfg, err := parsePruneOptions("prune", opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneWithConfig(ctx, r, eng, cfg)
}

func pruneWithConfig(ctx context.Context, r cli.Runner, eng engine, cfg pruneConfig) ([]string, error) {
	return pruneListedWithGroupConfig(ctx, r, eng, eng.listArgs(), eng.parseStoppedManaged, "prune", "", cfg)
}

type pruneCandidate struct {
	id         string
	labels     map[string]string
	creation   string
	createdAt  time.Time
	state      State
	managed    bool
	reuse      bool
	reuseGroup string
}

// pruneAgeFilter limits a prune run to candidates whose backend creation
// time is strictly before cutoff. A zero creation time is unknown and is
// never admitted: a missing timestamp is not evidence that a container is
// old enough to delete.
type pruneAgeFilter struct {
	active bool
	cutoff time.Time
}

func newPruneAgeFilter(cfg pruneConfig) pruneAgeFilter {
	if cfg.olderThan <= 0 {
		return pruneAgeFilter{}
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	return pruneAgeFilter{active: true, cutoff: now().Add(-cfg.olderThan)}
}

func (f pruneAgeFilter) admits(createdAt time.Time) bool {
	if !f.active {
		return true
	}
	return !createdAt.IsZero() && createdAt.Before(f.cutoff)
}

// pruneListed lists candidates and removes each still-current candidate.
// Apple candidates carry list-time ownership/state metadata and are
// revalidated while the per-name lock is held before a name is deleted.
func pruneListed(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]string, error), errKind string, opts ...PruneOption) ([]string, error) {
	cfg, err := parsePruneOptions(errKind, opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneListedWithGroupConfig(ctx, r, eng, listArgs, parse, errKind, "", cfg)
}

func pruneListedWithGroupConfig(ctx context.Context, r cli.Runner, eng engine, listArgs []string, parse func([]byte) ([]string, error), errKind, reuseGroup string, cfg pruneConfig) ([]string, error) {
	// Parsed entry points always supply positive defaults. Keep this helper
	// fail-safe for direct internal callers: zero parallelism would silently
	// remove nothing, and a non-positive timeout would expire immediately.
	if cfg.parallelism < 1 {
		cfg.parallelism = 1
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultPruneTimeout
	}
	ages := newPruneAgeFilter(cfg)

	redactor := cli.NewHashedContextRedactor(reuseGroup)
	overallCtx, overallCancel := context.WithTimeout(ctx, cfg.timeout)
	defer overallCancel()

	qCtx, cancel := context.WithTimeout(overallCtx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, listArgs...)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, cli.WithRedactor(classifyError(overallCtx, r, err, eng), redactor)
	}
	ids, err := parse(stdout)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, cli.WithRedactor(err, redactor)
	}

	if len(ids) == 0 {
		return nil, nil
	}

	var listed map[string]pruneCandidate
	if eng.name() == "apple" {
		listed, err = applePruneCandidates(stdout)
		if err != nil {
			return nil, err
		}
	}

	// parsePruneOptions rejects a non-positive parallelism, so cfg.parallelism
	// is always at least one here.
	concurrency := cfg.parallelism
	if len(ids) < concurrency {
		concurrency = len(ids)
	}

	type pruneJob struct {
		index int
		id    string
	}

	jobs := make(chan pruneJob, len(ids))
	for i, id := range ids {
		jobs <- pruneJob{index: i, id: id}
	}
	close(jobs)

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		didRemoveAt = make([]bool, len(ids))
		errAt       = make([]error, len(ids))
		attemptedAt = make([]bool, len(ids))
	)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if overallCtx.Err() != nil {
					return
				}
				var didRemove bool
				var opErr error
				if eng.name() == "apple" {
					candidate, ok := listed[job.id]
					if !ok {
						opErr = fmt.Errorf("%s %s: list candidate metadata missing", errKind, job.id)
					} else {
						didRemove, opErr = pruneNamedCandidateWithMetadata(overallCtx, r, eng, candidate, errKind, reuseGroup, ages)
					}
				} else {
					didRemove, opErr = pruneDockerCandidate(overallCtx, r, eng, job.id, errKind, reuseGroup, ages)
				}
				mu.Lock()
				attemptedAt[job.index] = true
				didRemoveAt[job.index] = didRemove
				errAt[job.index] = opErr
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	var removed []string
	var errs []error
	for i, id := range ids {
		if didRemoveAt[i] {
			removed = append(removed, id)
		}
		if errAt[i] != nil {
			errs = append(errs, errAt[i])
		}
	}

	if overallCtx.Err() != nil {
		unattempted := 0
		for i := range ids {
			if !attemptedAt[i] {
				unattempted++
			}
		}
		// Only remaining work makes the overall deadline a failure. When every
		// candidate was attempted, the per-candidate results already say what
		// happened, and a deadline that expired after the last one completed
		// must not turn a fully applied prune into an error.
		if unattempted > 0 {
			errs = append(errs, fmt.Errorf("%s: %d container(s) remaining: %w", errKind, unattempted, overallCtx.Err()))
		}
	}

	return removed, cli.WithRedactor(errors.Join(errs...), redactor)
}

func pruneDockerCandidate(ctx context.Context, r cli.Runner, eng engine, id, errKind, reuseGroup string, ages pruneAgeFilter) (bool, error) {
	if !dockerIDRE.MatchString(id) {
		return false, nil
	}
	// The verify and the delete together must not outlive one queryTimeout.
	// The overall prune deadline is longer (five minutes by default) and
	// withDefaultTimeout leaves a context that already has a deadline
	// unchanged, so bounding only the delete would let a hung inspect run to
	// the overall deadline instead.
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	inspectContainer := &Container{id: id, runner: r, eng: eng, sync: &lockPack{uid: id}}
	fresh, err := inspectContainer.inspectFresh(ctx)
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, id, contextErr)
	}
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
		createdAt:  fresh.createdAt,
		state:      fresh.state,
		managed:    fresh.labels[managedLabel] == "true",
		reuse:      fresh.labels[reuseLabel] == "true",
		reuseGroup: fresh.labels[reuseGroupLabel],
	}
	if !pruneCandidateEligible(candidate, reuseGroup) || !ages.admits(candidate.createdAt) {
		// In particular, ordinary prune never force-deletes a candidate
		// that became running after the daemon-side list.
		return false, nil
	}
	deleteArgs := eng.deleteArgs(id)
	_, _, err = r.Run(ctx, deleteArgs...)
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, id, contextErr)
	}
	if err != nil && !isNotFoundForOperation(eng, err, commandOperation(deleteArgs), id) {
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
			createdAt:  parseInspectTime(c.Configuration.CreationDate),
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
		if candidate.labels[key] != fresh.labels[key] {
			return false
		}
	}
	return true
}

func pruneNamedCandidateWithMetadata(ctx context.Context, r cli.Runner, eng engine, candidate pruneCandidate, errKind, reuseGroup string, ages pruneAgeFilter) (bool, error) {
	if eng.name() != "apple" || !pruneCandidateEligible(candidate, reuseGroup) {
		return false, nil
	}
	// The list-time timestamp can keep a known-recent candidate from taking
	// the name lock at all. Unknown list metadata is checked again against
	// the fresh inspect below.
	if ages.active && !candidate.createdAt.IsZero() && !ages.admits(candidate.createdAt) {
		return false, nil
	}
	guardCtx, guardCancel := context.WithTimeout(ctx, queryTimeout)
	defer guardCancel()
	unlock, err := lockName(guardCtx, candidate.id)
	if err != nil {
		return false, fmt.Errorf("%s %s: lock name: %w", errKind, candidate.id, err)
	}
	defer unlock()

	fresh, err := (&Container{id: candidate.id, runner: r, eng: eng, nameInspect: true}).inspectFreshLocked(guardCtx)
	if contextErr := commandContextError(guardCtx, err); contextErr != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, contextErr)
	}
	if isNotFoundFor(eng, err) {
		unregisterContainerReaper(&config{runner: r, eng: eng, name: candidate.id, creation: candidate.creation}, candidate.id, candidate.creation, "")
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: verify before delete: %w", errKind, candidate.id, err)
	}
	if !pruneCandidateStillCurrent(candidate, fresh, reuseGroup) || fresh.uid != "" || !ages.admits(fresh.createdAt) {
		return false, nil
	}
	deleteArgs := eng.deleteArgs(candidate.id)
	_, _, err = r.Run(guardCtx, deleteArgs...)
	if contextErr := commandContextError(guardCtx, err); contextErr != nil {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, contextErr)
	}
	if err != nil && !isNotFoundForOperation(eng, err, commandOperation(deleteArgs), candidate.id) {
		return false, fmt.Errorf("%s %s: %w", errKind, candidate.id, err)
	}
	unregisterContainerReaper(&config{runner: r, eng: eng, name: candidate.id, creation: candidate.creation}, candidate.id, candidate.creation, "")
	return true, nil
}
