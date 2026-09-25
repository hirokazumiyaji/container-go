package container

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// pruneSafetyRunner makes the list/inspect/delete sequence deterministic.
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

func pruneFixture(name, creation, state, group string, managed bool) pruneFixtureContainer {
	labels := map[string]string{}
	if managed {
		labels[managedLabel] = "true"
	}
	if creation != "" {
		labels[creationLabel] = creation
	}
	if group != "" {
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
	cases := []struct {
		name   string
		listed pruneFixtureContainer
		fresh  pruneFixtureContainer
	}{
		{
			name:   "list generation missing",
			listed: pruneFixture("shared", "", string(StateStopped), "", true),
			fresh:  pruneFixture("shared", "", string(StateStopped), "", true),
		},
		{
			name:   "fresh generation missing",
			listed: pruneFixture("shared", listedCreation, string(StateStopped), "", true),
			fresh:  pruneFixture("shared", "", string(StateStopped), "", true),
		},
		{
			name:   "generation mismatch",
			listed: pruneFixture("shared", listedCreation, string(StateStopped), "", true),
			fresh:  pruneFixture("shared", "bbbbbbbbbbbbbbbb", string(StateStopped), "", true),
		},
		{
			name:   "state changed",
			listed: pruneFixture("shared", listedCreation, string(StateStopped), "", true),
			fresh:  pruneFixture("shared", listedCreation, string(StateRunning), "", true),
		},
		{
			name:   "managed marker changed",
			listed: pruneFixture("shared", listedCreation, string(StateStopped), "", true),
			fresh:  pruneFixture("shared", listedCreation, string(StateStopped), "", false),
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
			if r.callCount("inspect") != 1 {
				t.Fatalf("inspect calls = %d, want one fresh inspect", r.callCount("inspect"))
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
	cases := []struct {
		name   string
		fresh  pruneFixtureContainer
		remove bool
	}{
		{
			name:   "matching candidate",
			fresh:  pruneFixture("shared", creation, string(StateRunning), "integration", true),
			remove: true,
		},
		{
			name:  "replacement generation",
			fresh: pruneFixture("shared", "bbbbbbbbbbbbbbbb", string(StateRunning), "integration", true),
		},
		{
			name:  "state changed",
			fresh: pruneFixture("shared", creation, string(StateStopped), "integration", true),
		},
		{
			name:  "group changed",
			fresh: pruneFixture("shared", creation, string(StateRunning), "other", true),
		},
		{
			name:  "managed marker missing",
			fresh: pruneFixture("shared", creation, string(StateRunning), "integration", false),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &pruneSafetyRunner{
				list:     []pruneFixtureContainer{pruneFixture("shared", creation, string(StateRunning), "integration", true)},
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

type dockerPruneIDRunner struct {
	mu      sync.Mutex
	list    string
	deleted []string
	calls   [][]string
}

func (r *dockerPruneIDRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "ps":
		return []byte(r.list), nil, nil
	case "rm":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func assertDockerFullIDListArgs(t *testing.T, args []string) {
	t.Helper()
	if !slices.Contains(args, "--no-trunc") || !slices.Contains(args, "{{.ID}}") {
		t.Fatalf("Docker list args = %v, want --no-trunc and {{.ID}}", args)
	}
	if slices.Contains(args, "{{.Names}}") {
		t.Fatalf("Docker list args use replaceable names: %v", args)
	}
}

func TestDockerPruneDeletesListedIDAfterNameReplacement(t *testing.T) {
	id := strings.Repeat("a", 64)
	r := &dockerPruneIDRunner{list: id + "\n"}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !slices.Equal(removed, []string{id}) {
		t.Fatalf("removed = %v, want full ID %q", removed, id)
	}
	if !slices.Equal(r.deleted, []string{id}) {
		t.Fatalf("deleted = %v, want the listed immutable ID", r.deleted)
	}
	assertDockerFullIDListArgs(t, r.calls[0])
}

func TestDockerPruneReuseGroupDeletesListedIDAfterNameReplacement(t *testing.T) {
	first := strings.Repeat("b", 64)
	second := strings.Repeat("c", 64)
	r := &dockerPruneIDRunner{list: first + "\n" + second + "\n"}
	removed, err := pruneReuseGroupWith(context.Background(), r, dockerEngine{}, "integration")
	if err != nil {
		t.Fatalf("PruneReuseGroup: %v", err)
	}
	if !slices.Equal(removed, []string{first, second}) {
		t.Fatalf("removed = %v, want full IDs", removed)
	}
	if !slices.Equal(r.deleted, []string{first, second}) {
		t.Fatalf("deleted = %v, want listed immutable IDs", r.deleted)
	}
	assertDockerFullIDListArgs(t, r.calls[0])
}

func TestDockerPruneRejectsTruncatedListedID(t *testing.T) {
	short := strings.Repeat("a", 12)
	r := &dockerPruneIDRunner{list: short + "\n"}
	if _, err := pruneWith(context.Background(), r, dockerEngine{}); err == nil {
		t.Fatal("Prune accepted a truncated Docker ID")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no delete after invalid list identity", r.deleted)
	}
}
