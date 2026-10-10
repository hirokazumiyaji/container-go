package container

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// pruneSafetyRunner makes the list/inspect/delete sequence deterministic.
// The list response describes the old candidate; inspect responses model
// whatever is live when the guarded delete reaches the backend.
type pruneSafetyRunner struct {
	mu          sync.Mutex
	list        []pruneFixtureContainer
	inspects    []pruneFixtureContainer
	inspectErrs []error
	deleteErr   error
	calls       [][]string
	deleted     []string
}

type pruneFixtureContainer struct {
	ID     string                    `json:"id"`
	Config pruneFixtureConfiguration `json:"configuration"`
	Status pruneFixtureStatus        `json:"status"`
}

type pruneFixtureConfiguration struct {
	Labels map[string]string `json:"labels"`
}

type pruneFixtureStatus struct {
	State string `json:"state"`
}

func (r *pruneSafetyRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)

	switch args[0] {
	case "ls":
		return mustPruneJSON(r.list), nil, nil
	case "inspect":
		n := 0
		for _, call := range r.calls {
			if call[0] == "inspect" {
				n++
			}
		}
		if n <= len(r.inspectErrs) && r.inspectErrs[n-1] != nil {
			return nil, nil, r.inspectErrs[n-1]
		}
		if len(r.inspects) == 0 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "container not found"}
		}
		container := r.inspects[len(r.inspects)-1]
		if n <= len(r.inspects) {
			container = r.inspects[n-1]
		}
		return mustPruneJSON([]pruneFixtureContainer{container}), nil, nil
	case "delete", "rm":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, r.deleteErr
	case "system", "version", "info":
		return []byte("ok"), nil, nil
	default:
		return nil, nil, nil
	}
}

func mustPruneJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

const pruneFixtureSession = "0123456789abcdef"

func pruneFixture(name, creation, state, group string, managed bool) pruneFixtureContainer {
	labels := map[string]string{
		sessionLabel: pruneFixtureSession,
	}
	if managed {
		labels[managedLabel] = "true"
	}
	if creation != "" {
		labels[creationLabel] = creation
	}
	if group != "" {
		labels[reuseLabel] = "true"
		labels[reuseGroupLabel] = group
	}
	return pruneFixtureContainer{
		ID: name,
		Config: pruneFixtureConfiguration{
			Labels: labels,
		},
		Status: pruneFixtureStatus{State: state},
	}
}

func (r *pruneSafetyRunner) callCount(subcommand string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if call[0] == subcommand {
			n++
		}
	}
	return n
}

func TestApplePruneParsersCaptureIdentityMetadata(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	fixture := pruneFixture("shared", creation, string(StateStopped), "integration", true)
	data := mustPruneJSON([]pruneFixtureContainer{fixture})

	candidates, err := applePruneCandidates(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v, want one", candidates)
	}
	candidate, ok := candidates["shared"]
	if !ok {
		t.Fatalf("candidate for shared not found: %+v", candidates)
	}
	if candidate.id != "shared" || candidate.creation != creation ||
		candidate.labels[sessionLabel] != pruneFixtureSession || candidate.state != StateStopped ||
		!candidate.managed || !candidate.reuse || candidate.reuseGroup != "integration" {
		t.Fatalf("candidate = %+v, want complete Apple identity metadata", candidate)
	}
}

func TestPruneAppleDoesNotDeleteReplacement(t *testing.T) {
	const oldCreation = "aaaaaaaaaaaaaaaa"
	const newCreation = "bbbbbbbbbbbbbbbb"
	r := &pruneSafetyRunner{
		list: []pruneFixtureContainer{
			pruneFixture("shared", oldCreation, string(StateStopped), "", true),
		},
		inspects: []pruneFixtureContainer{
			pruneFixture("shared", newCreation, string(StateRunning), "", true),
		},
	}

	removed, err := pruneWith(context.Background(), r, appleEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 0 || len(r.deleted) != 0 {
		t.Fatalf("removed = %v, deleted = %v; want replacement protected", removed, r.deleted)
	}
}

func TestPruneAppleFailsClosedOnUnverifiedCandidate(t *testing.T) {
	const listedCreation = "aaaaaaaaaaaaaaaa"
	matching := func() pruneFixtureContainer {
		return pruneFixture("shared", listedCreation, string(StateStopped), "", true)
	}
	running := matching()
	running.Status.State = string(StateRunning)
	withLabel := func(container pruneFixtureContainer, key, value string) pruneFixtureContainer {
		if value == "" {
			delete(container.Config.Labels, key)
		} else {
			container.Config.Labels[key] = value
		}
		return container
	}

	cases := []struct {
		name        string
		listed      pruneFixtureContainer
		fresh       pruneFixtureContainer
		wantInspect bool
	}{
		{
			name:   "list generation missing",
			listed: withLabel(matching(), creationLabel, ""),
			fresh:  matching(),
		},
		{
			name:   "list generation malformed",
			listed: withLabel(matching(), creationLabel, "not-a-generation"),
			fresh:  matching(),
		},
		{
			name:        "fresh generation missing",
			listed:      matching(),
			fresh:       withLabel(matching(), creationLabel, ""),
			wantInspect: true,
		},
		{
			name:        "generation mismatch",
			listed:      matching(),
			fresh:       withLabel(matching(), creationLabel, "bbbbbbbbbbbbbbbb"),
			wantInspect: true,
		},
		{
			name:        "list session missing",
			listed:      withLabel(matching(), sessionLabel, ""),
			fresh:       matching(),
			wantInspect: true,
		},
		{
			name:        "fresh session missing",
			listed:      matching(),
			fresh:       withLabel(matching(), sessionLabel, ""),
			wantInspect: true,
		},
		{
			name:        "session mismatch",
			listed:      matching(),
			fresh:       withLabel(matching(), sessionLabel, "bbbbbbbbbbbbbbbb"),
			wantInspect: true,
		},
		{
			name:        "state changed",
			listed:      matching(),
			fresh:       running,
			wantInspect: true,
		},
		{
			name:        "reuse marker changed",
			listed:      withLabel(matching(), reuseLabel, "true"),
			fresh:       withLabel(matching(), reuseLabel, ""),
			wantInspect: true,
		},
		{
			name:        "group changed",
			listed:      withLabel(withLabel(matching(), reuseGroupLabel, "old"), reuseLabel, "true"),
			fresh:       withLabel(withLabel(matching(), reuseGroupLabel, "new"), reuseLabel, "true"),
			wantInspect: true,
		},
		{
			name:        "managed marker changed",
			listed:      matching(),
			fresh:       withLabel(matching(), managedLabel, ""),
			wantInspect: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &pruneSafetyRunner{
				list:     []pruneFixtureContainer{tc.listed},
				inspects: []pruneFixtureContainer{tc.fresh},
			}
			removed, err := pruneWith(context.Background(), r, appleEngine{})
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if len(removed) != 0 || len(r.deleted) != 0 {
				t.Fatalf("removed = %v, deleted = %v; want fail-closed skip", removed, r.deleted)
			}
			wantInspect := 0
			if tc.wantInspect {
				wantInspect = 1
			}
			if r.callCount("inspect") != wantInspect {
				t.Fatalf("inspect calls = %d, want %d", r.callCount("inspect"), wantInspect)
			}
		})
	}
}

func TestPruneAppleDeletesOnlyWhenCandidateStillCurrent(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	r := &pruneSafetyRunner{
		list:     []pruneFixtureContainer{pruneFixture("shared", creation, string(StateStopped), "", true)},
		inspects: []pruneFixtureContainer{pruneFixture("shared", creation, string(StateStopped), "", true)},
	}

	removed, err := pruneWith(context.Background(), r, appleEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "shared" {
		t.Fatalf("removed = %v, want [shared]", removed)
	}
	if len(r.deleted) != 1 || r.deleted[0] != "shared" {
		t.Fatalf("deleted = %v, want [shared]", r.deleted)
	}
}

func TestPruneReuseGroupAppleUsesSameCandidateGuard(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	matching := func(state State) pruneFixtureContainer {
		return pruneFixture("shared", creation, string(state), "integration", true)
	}
	withLabel := func(container pruneFixtureContainer, key, value string) pruneFixtureContainer {
		if value == "" {
			delete(container.Config.Labels, key)
		} else {
			container.Config.Labels[key] = value
		}
		return container
	}

	cases := []struct {
		name   string
		listed pruneFixtureContainer
		fresh  pruneFixtureContainer
		remove bool
	}{
		{
			name:   "matching running candidate",
			listed: matching(StateRunning),
			fresh:  matching(StateRunning),
			remove: true,
		},
		{
			name:   "matching stopped candidate",
			listed: matching(StateStopped),
			fresh:  matching(StateStopped),
			remove: true,
		},
		{
			name:   "replacement generation",
			listed: matching(StateRunning),
			fresh:  withLabel(matching(StateRunning), creationLabel, "bbbbbbbbbbbbbbbb"),
		},
		{
			name:   "state changed",
			listed: matching(StateRunning),
			fresh:  matching(StateStopped),
		},
		{
			name:   "group changed",
			listed: matching(StateRunning),
			fresh:  withLabel(matching(StateRunning), reuseGroupLabel, "other"),
		},
		{
			name:   "list reuse marker missing",
			listed: withLabel(matching(StateRunning), reuseLabel, ""),
			fresh:  matching(StateRunning),
		},
		{
			name:   "fresh reuse marker missing",
			listed: matching(StateRunning),
			fresh:  withLabel(matching(StateRunning), reuseLabel, ""),
		},
		{
			name:   "list session missing",
			listed: withLabel(matching(StateRunning), sessionLabel, ""),
			fresh:  matching(StateRunning),
		},
		{
			name:   "fresh session changed",
			listed: matching(StateRunning),
			fresh:  withLabel(matching(StateRunning), sessionLabel, "bbbbbbbbbbbbbbbb"),
		},
		{
			name:   "list managed marker missing",
			listed: withLabel(matching(StateRunning), managedLabel, ""),
			fresh:  matching(StateRunning),
		},
		{
			name:   "fresh managed marker missing",
			listed: matching(StateRunning),
			fresh:  withLabel(matching(StateRunning), managedLabel, ""),
		},
		{
			name:   "created transitional state",
			listed: matching(StateCreated),
			fresh:  matching(StateCreated),
		},
		{
			name:   "stopping transitional state",
			listed: matching(StateStopping),
			fresh:  matching(StateStopping),
		},
		{
			name:   "unknown state",
			listed: matching(StateUnknown),
			fresh:  matching(StateUnknown),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &pruneSafetyRunner{
				list:     []pruneFixtureContainer{tc.listed},
				inspects: []pruneFixtureContainer{tc.fresh},
			}
			removed, err := pruneReuseGroupWith(context.Background(), r, appleEngine{}, "integration")
			if err != nil {
				t.Fatalf("PruneReuseGroup: %v", err)
			}
			if tc.remove {
				if len(removed) != 1 || len(r.deleted) != 1 || r.deleted[0] != "shared" {
					t.Fatalf("removed = %v, deleted = %v; want shared removed", removed, r.deleted)
				}
				return
			}
			if len(removed) != 0 || len(r.deleted) != 0 {
				t.Fatalf("removed = %v, deleted = %v; want guarded skip", removed, r.deleted)
			}
		})
	}
}

func TestPruneAppleReturnsInspectFailureWithoutDeleting(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	inspectErr := &cli.CLIError{Args: []string{"inspect", "shared"}, ExitCode: 1, Stderr: "daemon unavailable"}
	r := &pruneSafetyRunner{
		list:        []pruneFixtureContainer{pruneFixture("shared", creation, string(StateStopped), "", true)},
		inspectErrs: []error{inspectErr},
	}
	removed, err := pruneWith(context.Background(), r, appleEngine{})
	if err == nil || !strings.Contains(err.Error(), "verify before delete") {
		t.Fatalf("Prune = %v, want inspect failure", err)
	}
	if len(removed) != 0 || len(r.deleted) != 0 {
		t.Fatalf("removed = %v, deleted = %v; want fail closed", removed, r.deleted)
	}
}

type pruneLockScopeRunner struct {
	mu             sync.Mutex
	name           string
	inspectStarted chan struct{}
	releaseInspect chan struct{}
	deleteObserved chan struct{}
	inspectOnce    sync.Once
	deleteOnce     sync.Once
	calls          []string
}

func (r *pruneLockScopeRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(args, " "))
	r.mu.Unlock()

	switch args[0] {
	case "ls":
		return mustPruneJSON([]pruneFixtureContainer{
			pruneFixture(r.name, "aaaaaaaaaaaaaaaa", string(StateStopped), "", true),
		}), nil, nil
	case "inspect":
		r.inspectOnce.Do(func() { close(r.inspectStarted) })
		select {
		case <-r.releaseInspect:
			return mustPruneJSON([]pruneFixtureContainer{
				pruneFixture(r.name, "aaaaaaaaaaaaaaaa", string(StateStopped), "", true),
			}), nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	case "delete":
		r.deleteOnce.Do(func() { close(r.deleteObserved) })
		return nil, nil, nil
	case "run":
		return []byte(r.name + "\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *pruneLockScopeRunner) callSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestPruneAppleHoldsNameLockThroughInspectAndDelete(t *testing.T) {
	name := "prune-lock-scope-" + newContainerName()
	runner := &pruneLockScopeRunner{
		name:           name,
		inspectStarted: make(chan struct{}),
		releaseInspect: make(chan struct{}),
		deleteObserved: make(chan struct{}),
	}
	pruneDone := make(chan error, 1)
	go func() {
		_, err := pruneWith(context.Background(), runner, appleEngine{})
		pruneDone <- err
	}()
	select {
	case <-runner.inspectStarted:
	case <-time.After(time.Second):
		t.Fatal("prune did not reach fresh inspect")
	}

	createDone := make(chan error, 1)
	go func() {
		cfg := &config{name: name, runner: runner, eng: appleEngine{}}
		_, _, attempted, err := runCreateLocked(context.Background(), cfg, "run")
		if err == nil && !attempted {
			err = errors.New("create was not attempted")
		}
		createDone <- err
	}()
	select {
	case err := <-createDone:
		t.Fatalf("create escaped the prune critical section: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(runner.releaseInspect)
	select {
	case err := <-pruneDone:
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prune did not finish after inspect release")
	}
	select {
	case <-runner.deleteObserved:
	case <-time.After(time.Second):
		t.Fatal("prune did not issue delete")
	}
	select {
	case err := <-createDone:
		if err != nil {
			t.Fatalf("create after prune: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("create remained blocked after prune delete")
	}

	calls := runner.callSnapshot()
	if len(calls) < 4 || calls[len(calls)-1] != "run" {
		t.Fatalf("calls = %v, want inspect then delete before create", calls)
	}
}
