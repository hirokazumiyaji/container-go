package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	cleanupDockerIDOne = "1111111111111111111111111111111111111111111111111111111111111111"
	cleanupDockerIDTwo = "2222222222222222222222222222222222222222222222222222222222222222"
)

func TestTerminateContainerIsNilSafe(t *testing.T) {
	if err := TerminateContainer(nil); err != nil {
		t.Fatalf("TerminateContainer(nil) = %v, want nil", err)
	}
}

func TestTerminateContainerDeletes(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	if err := TerminateContainer(ctr); err != nil {
		t.Fatalf("TerminateContainer: %v", err)
	}
	if f.callWith("delete") == nil {
		t.Error("delete not issued")
	}
}

func TestTerminateContainerHonorsKeepEnv(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	t.Setenv("CONTAINERGO_KEEP", "1")
	if err := TerminateContainer(ctr); err != nil {
		t.Fatalf("TerminateContainer: %v", err)
	}
	if f.callWith("delete") != nil {
		t.Error("delete issued despite CONTAINERGO_KEEP=1")
	}
}

func TestCleanupRunsAtTestEnd(t *testing.T) {
	f := newTestRunner()
	t.Run("inner", func(t *testing.T) {
		ctr := runTestContainer(t, f)
		Cleanup(t, ctr)
		if f.callWith("delete") != nil {
			t.Error("delete ran before test end")
		}
	})
	if f.callWith("delete") == nil {
		t.Error("delete did not run at test end")
	}
}

func TestCleanupIsNilSafe(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		Cleanup(t, nil) // must not panic at cleanup time
	})
}

// lsRunner serves a canned `ls` listing and records deletes.
type lsRunner struct {
	*fakeRunner
	lsJSON string
}

func (l *lsRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ls" {
		l.mu.Lock()
		l.calls = append(l.calls, args)
		l.mu.Unlock()
		return []byte(l.lsJSON), nil, nil
	}
	if args[0] == "inspect" && l.lsJSON != "" {
		l.mu.Lock()
		l.calls = append(l.calls, args)
		l.mu.Unlock()
		return []byte(l.lsJSON), nil, nil
	}
	return l.fakeRunner.Run(ctx, args...)
}

const pruneLsJSON = `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-running","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"running","networks":[]}},
  {"id":"unmanaged-stopped","configuration":{"labels":{}},"status":{"state":"stopped","networks":[]}}
]`

func TestPruneRemovesOnlyManagedStoppedContainers(t *testing.T) {
	f := &lsRunner{fakeRunner: newTestRunner(), lsJSON: pruneLsJSON}

	removed, err := pruneWith(context.Background(), f, appleEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !slices.Equal(removed, []string{"managed-stopped"}) {
		t.Errorf("removed = %v, want [managed-stopped]", removed)
	}

	lsCall := f.callWith("ls")
	if !slices.Contains(lsCall, "--all") {
		t.Errorf("ls call missing --all: %v", lsCall)
	}
	var deleted []string
	for _, call := range f.calls {
		if call[0] == "delete" {
			deleted = append(deleted, call[len(call)-1])
		}
	}
	if !slices.Equal(deleted, []string{"managed-stopped"}) {
		t.Errorf("deleted = %v", deleted)
	}
}

type dockerListRunner struct {
	*fakeRunner
	output    string
	deleteErr error
}

func (d *dockerListRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		d.mu.Lock()
		d.calls = append(d.calls, args)
		d.mu.Unlock()
		return []byte(d.output), nil, nil
	}
	if args[0] == "inspect" {
		d.mu.Lock()
		d.calls = append(d.calls, args)
		d.mu.Unlock()
		id := args[len(args)-1]
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:"integration"}},"NetworkSettings":{}}]`, id, managedLabel, reuseLabel, creationLabel, reuseGroupLabel)), nil, nil
	}
	if args[0] == "rm" && d.deleteErr != nil {
		d.mu.Lock()
		d.calls = append(d.calls, args)
		d.mu.Unlock()
		return nil, nil, d.deleteErr
	}
	return d.fakeRunner.Run(ctx, args...)
}

func TestDockerPruneAndReuseGroupUseVolumeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *dockerListRunner) ([]string, error)
	}{
		{
			name: "prune",
			run: func(ctx context.Context, r *dockerListRunner) ([]string, error) {
				return pruneWith(ctx, r, dockerEngine{})
			},
		},
		{
			name: "reuse group",
			run: func(ctx context.Context, r *dockerListRunner) ([]string, error) {
				return pruneReuseGroupWith(ctx, r, dockerEngine{}, "integration")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &dockerListRunner{fakeRunner: newTestRunner(), output: cleanupDockerIDOne + "\n" + cleanupDockerIDTwo + "\n"}
			removed, err := tc.run(context.Background(), r)
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if !slices.Equal(removed, []string{cleanupDockerIDOne, cleanupDockerIDTwo}) {
				t.Fatalf("removed = %v", removed)
			}

			var deletes [][]string
			for _, call := range r.calls {
				if call[0] == "rm" {
					deletes = append(deletes, call)
				}
			}
			if len(deletes) != 2 {
				t.Fatalf("rm calls = %v, want 2", deletes)
			}
			for _, call := range deletes {
				want := []string{"rm", "--force", "--volumes", call[len(call)-1]}
				if !slices.Equal(call, want) {
					t.Errorf("rm call = %v, want %v", call, want)
				}
			}
		})
	}
}

func TestDockerPruneReportsVolumeDeleteFailure(t *testing.T) {
	r := &dockerListRunner{
		fakeRunner: newTestRunner(),
		output:     cleanupDockerIDOne + "\n",
		deleteErr: &cli.CLIError{
			Binary: "docker",
			Args:   []string{"rm", "--force", "--volumes", cleanupDockerIDOne},
			Stderr: "error removing volume: volume driver plugin not found",
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err == nil {
		t.Fatal("prune succeeded, want volume deletion failure")
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want no falsely successful deletions", removed)
	}
	if !strings.Contains(err.Error(), "prune "+cleanupDockerIDOne) {
		t.Fatalf("error = %v, want prune target context", err)
	}
}

func TestDockerPruneAcceptsPreciseContainerNotFound(t *testing.T) {
	id := cleanupDockerIDOne
	r := &dockerListRunner{
		fakeRunner: newTestRunner(),
		output:     id + "\n",
		deleteErr: &cli.CLIError{
			Binary: "docker",
			Args:   []string{"rm", "--force", "--volumes", id},
			Stderr: "Error response from daemon: No such container: " + id,
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{id}) {
		t.Fatalf("removed = %v, want [%s]", removed, id)
	}
}

func TestDockerPruneSelectsExitedAndDeadOnly(t *testing.T) {
	args := (dockerEngine{}).listArgs()
	for _, want := range []string{"status=exited", "status=dead"} {
		if !slices.Contains(args, want) {
			t.Errorf("listArgs = %v, missing %q", args, want)
		}
	}
	for _, unwanted := range []string{"status=created", "status=running"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("listArgs = %v, unexpectedly includes %q", args, unwanted)
		}
	}
}

type cleanupTBRecorder struct {
	testing.TB
	callbacks []func()
	logs      []string
	errors    []string
	helpers   int
}

func (r *cleanupTBRecorder) Helper() { r.helpers++ }

func (r *cleanupTBRecorder) Cleanup(fn func()) {
	r.callbacks = append(r.callbacks, fn)
}

func (r *cleanupTBRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *cleanupTBRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestCleanupFunctionsMarkThemselvesAsHelpers(t *testing.T) {
	tb := &cleanupTBRecorder{}
	Cleanup(tb, nil)
	if tb.helpers < 2 {
		t.Fatalf("Cleanup helper calls = %d, want exported and registration helpers", tb.helpers)
	}

	tb = &cleanupTBRecorder{}
	StrictCleanup(tb, nil)
	if tb.helpers < 2 {
		t.Fatalf("StrictCleanup helper calls = %d, want exported and registration helpers", tb.helpers)
	}
}

func TestStrictCleanupReportsCleanupFailure(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.failPrefix = "delete"
	tb := &cleanupTBRecorder{}
	registerCleanup(tb, ctr, true)
	if len(tb.callbacks) != 1 {
		t.Fatalf("callbacks = %d, want 1", len(tb.callbacks))
	}
	tb.callbacks[0]()
	if len(tb.errors) != 1 {
		t.Fatalf("errors = %v, want cleanup failure", tb.errors)
	}
	if len(tb.logs) != 0 {
		t.Fatalf("logs = %v, strict cleanup should report through Errorf", tb.logs)
	}
}

func TestNonStrictCleanupLogsCleanupFailure(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.failPrefix = "delete"
	tb := &cleanupTBRecorder{}
	registerCleanup(tb, ctr, false)
	if len(tb.callbacks) != 1 {
		t.Fatalf("callbacks = %d, want 1", len(tb.callbacks))
	}
	tb.callbacks[0]()
	if len(tb.errors) != 0 {
		t.Fatalf("errors = %v, non-strict cleanup should not report through Errorf", tb.errors)
	}
	if len(tb.logs) != 1 {
		t.Fatalf("logs = %v, want exactly 1 log from non-strict cleanup failure", tb.logs)
	}
	if !strings.Contains(tb.logs[0], "cleanup") || !strings.Contains(tb.logs[0], ctr.ID()) {
		t.Fatalf("log = %q, want cleanup message mentioning container ID", tb.logs[0])
	}
}

func TestSessionLabelValueIsValid(t *testing.T) {
	id := sessionID()
	if len(id) != 16 || strings.ToLower(id) != id {
		t.Errorf("sessionID = %q, want 16 lowercase hex chars", id)
	}
}

func TestRunWaitFailureHonorsKeepEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "default rolls back", keep: "", wantDelete: true},
		{name: "keep skips rollback", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				withRunner(f), withEngine(appleEngine{}),
				WithWaitStrategy(&recordingStrategy{err: errors.New("never ready")}))
			if err == nil {
				t.Fatal("Run succeeded; want readiness wait error")
			}
			didDelete := f.callWith("delete") != nil
			if didDelete != tc.wantDelete {
				t.Fatalf("delete issued = %v, want %v", didDelete, tc.wantDelete)
			}
		})
	}
}

func TestRunCopyFailureHonorsKeepEnv(t *testing.T) {
	src := filepath.Join(t.TempDir(), "testfile")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "default rolls back", keep: "", wantDelete: true},
		{name: "keep skips rollback", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
			f := newTestRunner()
			f.failPrefix = "cp"
			_, err := Run(context.Background(), "redis:7-alpine",
				withRunner(f), withEngine(appleEngine{}),
				WithFiles(File{HostPath: src, ContainerPath: "/testfile"}))
			if err == nil {
				t.Fatal("Run succeeded; want copy error")
			}
			didDelete := f.callWith("delete") != nil
			if didDelete != tc.wantDelete {
				t.Fatalf("delete issued = %v, want %v", didDelete, tc.wantDelete)
			}
		})
	}
}

func TestRunFailedCreateHonorsKeepEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "default cleans up", keep: "", wantDelete: true},
		{name: "keep skips cleanup", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
			base := newTestRunner()
			base.imagePresent = true
			r := &failRunRunner{
				fakeRunner:  base,
				runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
				inspectJSON: ownedInspectJSON("myctr"),
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
			if err == nil {
				t.Fatal("Run succeeded; want run error")
			}
			didDelete := len(r.deleted) > 0
			if didDelete != tc.wantDelete {
				t.Fatalf("delete issued = %v, want %v", didDelete, tc.wantDelete)
			}
		})
	}
}

func TestExplicitTerminateIgnoresKeepEnv(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if f.callWith("delete") == nil {
		t.Fatal("explicit Terminate did not issue delete when CONTAINERGO_KEEP=1")
	}
}

func TestPruneOptionsValidation(t *testing.T) {
	tests := []struct {
		name    string
		opt     PruneOption
		wantErr string
	}{
		{
			name:    "negative timeout",
			opt:     WithPruneTimeout(-time.Second),
			wantErr: "timeout must be positive",
		},
		{
			name:    "zero timeout",
			opt:     WithPruneTimeout(0),
			wantErr: "timeout must be positive",
		},
		{
			name:    "negative parallelism",
			opt:     WithPruneParallelism(-1),
			wantErr: "parallelism must be positive",
		},
		{
			name:    "zero parallelism",
			opt:     WithPruneParallelism(0),
			wantErr: "parallelism must be positive",
		},
		{
			name:    "negative older than",
			opt:     WithPruneOlderThan(-time.Second),
			wantErr: "age must be positive",
		},
		{
			name:    "zero older than",
			opt:     WithPruneOlderThan(0),
			wantErr: "age must be positive",
		},
		{
			name:    "nil option",
			opt:     nil,
			wantErr: "option 0 is nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := pruneWith(context.Background(), f, appleEngine{}, tt.opt)
			if err == nil {
				t.Fatalf("pruneWith succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWithPruneOlderThanSetsConfig(t *testing.T) {
	cfg, err := parsePruneOptions("Prune", []PruneOption{WithPruneOlderThan(2 * time.Hour)})
	if err != nil {
		t.Fatalf("parsePruneOptions: %v", err)
	}
	if cfg.olderThan != 2*time.Hour {
		t.Fatalf("olderThan = %v, want 2h", cfg.olderThan)
	}
	if cfg.now == nil {
		t.Fatal("parsed prune config has no clock")
	}
}

// pruneAgeDockerRunner serves list IDs and per-container Docker inspect
// creation times. Tests supply the clock through pruneConfig.
type pruneAgeDockerRunner struct {
	*fakeRunner
	ids     []string
	created map[string]time.Time
}

func (r *pruneAgeDockerRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.recordCall(args)
	switch args[0] {
	case "ps":
		return []byte(strings.Join(r.ids, "\n") + "\n"), nil, nil
	case "inspect":
		id := args[len(args)-1]
		created := ""
		if at, ok := r.created[id]; ok && !at.IsZero() {
			created = fmt.Sprintf(`"Created":%q,`, at.UTC().Format(time.RFC3339Nano))
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/prune-age",%s"State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"0123456789abcdef"}},"NetworkSettings":{}}]`,
			id, created, managedLabel, creationLabel)), nil, nil
	case "rm":
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *pruneAgeDockerRunner) deletedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, call := range r.calls {
		if call[0] == "rm" {
			ids = append(ids, call[len(call)-1])
		}
	}
	return ids
}

func TestPruneOlderThanFiltersDockerCandidates(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	oldID := cleanupDockerIDOne
	boundaryID := cleanupDockerIDTwo
	recentID := strings.Repeat("3", 64)
	runner := &pruneAgeDockerRunner{
		fakeRunner: newTestRunner(),
		ids:        []string{oldID, boundaryID, recentID},
		created: map[string]time.Time{
			oldID:      now.Add(-2 * time.Hour),
			boundaryID: now.Add(-time.Hour),
			recentID:   now.Add(-30 * time.Minute),
		},
	}

	removed, err := pruneWithConfig(context.Background(), runner, dockerEngine{}, pruneConfig{
		olderThan: time.Hour,
		now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("pruneWithConfig: %v", err)
	}
	if !slices.Equal(removed, []string{oldID}) {
		t.Fatalf("removed = %v, want only the candidate older than the cutoff", removed)
	}
	if got := runner.deletedIDs(); !slices.Equal(got, []string{oldID}) {
		t.Fatalf("deleted = %v, want only %s", got, oldID)
	}
}

func TestPruneOlderThanKeepsDockerCandidateWithoutCreationTime(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	runner := &pruneAgeDockerRunner{
		fakeRunner: newTestRunner(),
		ids:        []string{cleanupDockerIDOne},
		created:    map[string]time.Time{},
	}

	removed, err := pruneWithConfig(context.Background(), runner, dockerEngine{}, pruneConfig{
		olderThan: time.Hour,
		now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("pruneWithConfig: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want a candidate with unknown creation time retained", removed)
	}
	if got := runner.deletedIDs(); len(got) != 0 {
		t.Fatalf("deleted = %v, want no deletion", got)
	}
}

func pruneFixtureCreated(container pruneFixtureContainer, at time.Time) pruneFixtureContainer {
	container.Config.CreationDate = at.UTC().Format(time.RFC3339Nano)
	return container
}

func TestPruneOlderThanFiltersAppleCandidates(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	old := pruneFixtureCreated(
		pruneFixture("old", "aaaaaaaaaaaaaaaa", string(StateStopped), "", true),
		now.Add(-2*time.Hour),
	)
	recent := pruneFixtureCreated(
		pruneFixture("recent", "bbbbbbbbbbbbbbbb", string(StateStopped), "", true),
		now.Add(-30*time.Minute),
	)
	runner := &pruneSafetyRunner{
		list:     []pruneFixtureContainer{old, recent},
		inspects: []pruneFixtureContainer{old, recent},
	}

	removed, err := pruneWithConfig(context.Background(), runner, appleEngine{}, pruneConfig{
		olderThan: time.Hour,
		now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("pruneWithConfig: %v", err)
	}
	if !slices.Equal(removed, []string{"old"}) {
		t.Fatalf("removed = %v, want only the candidate older than the cutoff", removed)
	}
	if !slices.Equal(runner.deleted, []string{"old"}) {
		t.Fatalf("deleted = %v, want only old", runner.deleted)
	}
	if got := runner.callCount("inspect"); got != 1 {
		t.Fatalf("inspect calls = %d, want only the admitted candidate inspected", got)
	}
}

func TestPruneOlderThanUsesAppleInspectTimeWhenListTimeIsMissing(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	listed := pruneFixture("old", "aaaaaaaaaaaaaaaa", string(StateStopped), "", true)
	fresh := pruneFixtureCreated(listed, now.Add(-2*time.Hour))
	runner := &pruneSafetyRunner{
		list:     []pruneFixtureContainer{listed},
		inspects: []pruneFixtureContainer{fresh},
	}

	removed, err := pruneWithConfig(context.Background(), runner, appleEngine{}, pruneConfig{
		olderThan: time.Hour,
		now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("pruneWithConfig: %v", err)
	}
	if !slices.Equal(removed, []string{"old"}) {
		t.Fatalf("removed = %v, want the inspect-reported old candidate removed", removed)
	}
}

func TestPruneOlderThanKeepsAppleCandidateThatBecameRecent(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	listed := pruneFixtureCreated(
		pruneFixture("shared", "aaaaaaaaaaaaaaaa", string(StateStopped), "", true),
		now.Add(-2*time.Hour),
	)
	fresh := pruneFixtureCreated(listed, now.Add(-30*time.Minute))
	runner := &pruneSafetyRunner{
		list:     []pruneFixtureContainer{listed},
		inspects: []pruneFixtureContainer{fresh},
	}

	removed, err := pruneWithConfig(context.Background(), runner, appleEngine{}, pruneConfig{
		olderThan: time.Hour,
		now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("pruneWithConfig: %v", err)
	}
	if len(removed) != 0 || len(runner.deleted) != 0 {
		t.Fatalf("removed = %v, deleted = %v; want the fresh recent timestamp retained", removed, runner.deleted)
	}
}

type blockingPruneRunner struct {
	*fakeRunner
	active    atomic.Int32
	maxActive atomic.Int32
	barrier   chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	ids       []string
}

func (b *blockingPruneRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		b.mu.Lock()
		b.calls = append(b.calls, args)
		b.mu.Unlock()
		return []byte(strings.Join(b.ids, "\n") + "\n"), nil, nil
	}
	if args[0] == "inspect" {
		b.mu.Lock()
		b.calls = append(b.calls, args)
		b.mu.Unlock()
		id := args[len(args)-1]
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:"integration"}},"NetworkSettings":{}}]`, id, managedLabel, reuseLabel, creationLabel, reuseGroupLabel)), nil, nil
	}
	if args[0] == "rm" {
		cur := b.active.Add(1)
		for {
			old := b.maxActive.Load()
			if cur <= old || b.maxActive.CompareAndSwap(old, cur) {
				break
			}
		}
		b.enterOnce.Do(func() { close(b.entered) })
		select {
		case <-b.barrier:
		case <-ctx.Done():
			b.active.Add(-1)
			return nil, nil, ctx.Err()
		}
		b.active.Add(-1)
		b.mu.Lock()
		b.calls = append(b.calls, args)
		b.mu.Unlock()
		return nil, nil, nil
	}
	return b.fakeRunner.Run(ctx, args...)
}

func TestPruneConcurrentDeletion(t *testing.T) {
	ids := []string{
		cleanupDockerIDOne,
		cleanupDockerIDTwo,
		strings.Repeat("3", 64),
		strings.Repeat("4", 64),
	}

	runner := &blockingPruneRunner{
		fakeRunner: newTestRunner(),
		barrier:    make(chan struct{}),
		entered:    make(chan struct{}),
		ids:        ids,
	}

	resCh := make(chan []string, 1)
	errCh := make(chan error, 1)
	go func() {
		removed, err := pruneWith(context.Background(), runner, dockerEngine{}, WithPruneParallelism(4))
		resCh <- removed
		errCh <- err
	}()

	<-runner.entered
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runner.maxActive.Load() > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(runner.barrier)

	err := <-errCh
	removed := <-resCh
	if err != nil {
		t.Fatalf("pruneWith failed: %v", err)
	}
	if len(removed) != len(ids) {
		t.Fatalf("removed = %v, want all %d ids", removed, len(ids))
	}
	if max := runner.maxActive.Load(); max < 2 {
		t.Fatalf("max concurrent rm operations = %d, want >= 2", max)
	}
}

func TestPruneOverallTimeoutReportsRemaining(t *testing.T) {
	ids := []string{
		cleanupDockerIDOne,
		cleanupDockerIDTwo,
		strings.Repeat("3", 64),
		strings.Repeat("4", 64),
	}

	runner := &blockingPruneRunner{
		fakeRunner: newTestRunner(),
		barrier:    make(chan struct{}), // never closed
		entered:    make(chan struct{}),
		ids:        ids,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	removed, err := pruneWith(ctx, runner, dockerEngine{}, WithPruneParallelism(2))
	if err == nil {
		t.Fatal("pruneWith succeeded, want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded cause", err)
	}
	if !strings.Contains(err.Error(), "remaining") {
		t.Fatalf("err = %v, want remaining count mentioned", err)
	}
	if len(removed) > 0 {
		t.Fatalf("removed = %v, want empty on timeout before any complete", removed)
	}
}

func TestPruneTimeoutCapsCallerWithLaterDeadline(t *testing.T) {
	ids := []string{cleanupDockerIDOne}
	runner := &blockingPruneRunner{
		fakeRunner: newTestRunner(),
		barrier:    make(chan struct{}), // never closed
		entered:    make(chan struct{}),
		ids:        ids,
	}

	// Caller supplies a 1-hour deadline, but WithPruneTimeout is 50ms.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	start := time.Now()
	_, err := pruneWith(ctx, runner, dockerEngine{}, WithPruneTimeout(50*time.Millisecond))
	if err == nil {
		t.Fatal("pruneWith succeeded, want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("prune took %v, want bounded by WithPruneTimeout (50ms)", elapsed)
	}
}

// inspectDeadlinePruneRunner records the deadline the verify step is handed.
type inspectDeadlinePruneRunner struct {
	*fakeRunner
	inspectDeadline time.Time
}

func (r *inspectDeadlinePruneRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte(cleanupDockerIDOne + "\n"), nil, nil
	case "inspect":
		r.inspectDeadline, _ = ctx.Deadline()
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef"}},"NetworkSettings":{}}]`,
			cleanupDockerIDOne, managedLabel, reuseLabel, creationLabel)), nil, nil
	case "rm":
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

// TestPruneDockerCandidateIsBoundedByQueryTimeout pins the per-candidate bound
// on the docker path. The overall prune deadline is deliberately an hour, and
// withDefaultTimeout leaves a context that already carries a deadline
// unchanged, so the verify step has to be capped explicitly or a hung inspect
// runs until the overall deadline instead of queryTimeout.
func TestPruneDockerCandidateIsBoundedByQueryTimeout(t *testing.T) {
	runner := &inspectDeadlinePruneRunner{fakeRunner: newTestRunner()}
	removed, err := pruneWith(context.Background(), runner, dockerEngine{}, WithPruneTimeout(time.Hour))
	if err != nil {
		t.Fatalf("pruneWith: %v", err)
	}
	if len(removed) != 1 || removed[0] != cleanupDockerIDOne {
		t.Fatalf("removed = %v, want [%s]", removed, cleanupDockerIDOne)
	}
	if runner.inspectDeadline.IsZero() {
		t.Fatal("the verify step never ran")
	}
	if remaining := time.Until(runner.inspectDeadline); remaining > queryTimeout {
		t.Fatalf("verify deadline is %v away, want at most queryTimeout (%v)", remaining, queryTimeout)
	}
}
