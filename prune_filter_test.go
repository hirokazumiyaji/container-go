package container

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

type pruneLabelDockerRunner struct {
	*fakeRunner
	ids      []string
	labels   map[string]string
	mu       sync.Mutex
	psCalls  [][]string
	rmCalls  [][]string
	inspects int
}

func (r *pruneLabelDockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		r.mu.Lock()
		r.psCalls = append(r.psCalls, args)
		r.mu.Unlock()
		return []byte(strings.Join(r.ids, "\n") + "\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		r.inspects++
		r.mu.Unlock()
		id := args[len(args)-1]
		labels, err := json.Marshal(r.labels)
		if err != nil {
			return nil, nil, err
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/c","State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":%s},"NetworkSettings":{}}]`, id, labels)), nil, nil
	case "rm":
		r.mu.Lock()
		r.rmCalls = append(r.rmCalls, args)
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestPruneLabelFiltersDockerCandidatesAndListQuery(t *testing.T) {
	r := &pruneLabelDockerRunner{
		fakeRunner: newTestRunner(),
		ids:        []string{cleanupDockerIDOne},
		labels: map[string]string{
			managedLabel:  "true",
			creationLabel: "0123456789abcdef",
			"team":        "alpha",
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{}, WithPruneLabel("team", "alpha"))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{cleanupDockerIDOne}) {
		t.Fatalf("removed = %v, want [%s]", removed, cleanupDockerIDOne)
	}
	if len(r.psCalls) != 1 || !slices.Contains(r.psCalls[0], "label=team=alpha") {
		t.Fatalf("ps args = %v, want a daemon-side label filter", r.psCalls)
	}
	if len(r.rmCalls) != 1 {
		t.Fatalf("rm calls = %v, want 1", r.rmCalls)
	}
}

func TestPruneLabelRechecksDockerCandidateLabels(t *testing.T) {
	r := &pruneLabelDockerRunner{
		fakeRunner: newTestRunner(),
		ids:        []string{cleanupDockerIDOne},
		labels: map[string]string{
			managedLabel:  "true",
			creationLabel: "0123456789abcdef",
			"team":        "beta",
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{}, WithPruneLabel("team", "alpha"))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want no candidate with a mismatched label", removed)
	}
	if len(r.rmCalls) != 0 {
		t.Fatalf("rm calls = %v, want none", r.rmCalls)
	}
}

func TestPruneLabelEmptyValueRequiresPresence(t *testing.T) {
	r := &pruneLabelDockerRunner{
		fakeRunner: newTestRunner(),
		ids:        []string{cleanupDockerIDOne},
		labels: map[string]string{
			managedLabel:  "true",
			creationLabel: "0123456789abcdef",
			"team":        "",
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{}, WithPruneLabel("team", ""))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{cleanupDockerIDOne}) {
		t.Fatalf("removed = %v, want the presence-matched candidate", removed)
	}
	if !slices.Contains(r.psCalls[0], "label=team") {
		t.Fatalf("ps args = %v, want the presence filter", r.psCalls)
	}
}

func TestPruneDryRunDockerVerifiesWithoutDeleting(t *testing.T) {
	r := &pruneLabelDockerRunner{
		fakeRunner: newTestRunner(),
		ids:        []string{cleanupDockerIDOne},
		labels: map[string]string{
			managedLabel:  "true",
			creationLabel: "0123456789abcdef",
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{}, WithPruneDryRun())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{cleanupDockerIDOne}) {
		t.Fatalf("removed = %v, want the candidate a real prune would remove", removed)
	}
	if r.inspects != 1 {
		t.Fatalf("inspect calls = %d, want candidate verification", r.inspects)
	}
	if len(r.rmCalls) != 0 {
		t.Fatalf("rm calls = %v, want none for a dry run", r.rmCalls)
	}
}

func TestPruneLabelFiltersAppleCandidates(t *testing.T) {
	lsJSON := `[
  {"id":"managed-alpha","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef","team":"alpha"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-beta","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef","team":"beta"}},"status":{"state":"stopped","networks":[]}}
]`
	f := &lsRunner{fakeRunner: newTestRunner(), lsJSON: lsJSON}
	removed, err := pruneWith(context.Background(), f, appleEngine{}, WithPruneLabel("team", "alpha"))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{"managed-alpha"}) {
		t.Fatalf("removed = %v, want [managed-alpha]", removed)
	}
	var deleted []string
	for _, call := range f.calls {
		if call[0] == "delete" {
			deleted = append(deleted, call[len(call)-1])
		}
	}
	if !slices.Equal(deleted, []string{"managed-alpha"}) {
		t.Fatalf("deleted = %v, want [managed-alpha]", deleted)
	}
	if args := f.callWith("ls"); slices.Contains(args, "label=team=alpha") {
		t.Errorf("Apple ls args = %v, want client-side label filtering", args)
	}
}

func TestPruneOptionsLabelValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opt     PruneOption
		wantErr string
	}{
		{name: "empty key", opt: WithPruneLabel("", "x"), wantErr: "invalid label key"},
		{name: "invalid key", opt: WithPruneLabel("bad key", "x"), wantErr: "invalid label key"},
		{name: "NUL value", opt: WithPruneLabel("team", "a\x00b"), wantErr: "must not contain NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pruneWith(context.Background(), newTestRunner(), appleEngine{}, tc.opt)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPruneDryRunAppleVerifiesWithoutDeleting(t *testing.T) {
	lsJSON := `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef","team":"alpha"}},"status":{"state":"stopped","networks":[]}}
]`
	f := &lsRunner{fakeRunner: newTestRunner(), lsJSON: lsJSON}
	removed, err := pruneWith(context.Background(), f, appleEngine{}, WithPruneDryRun())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{"managed-stopped"}) {
		t.Fatalf("removed = %v, want [managed-stopped]", removed)
	}
	if f.callWith("delete") != nil {
		t.Error("dry run issued a delete")
	}
}

func TestPruneReuseGroupAppliesLabelFilter(t *testing.T) {
	r := &dockerListRunner{fakeRunner: newTestRunner(), output: cleanupDockerIDOne + "\n"}
	removed, err := pruneReuseGroupWith(context.Background(), r, dockerEngine{}, "integration", WithPruneLabel("team", "alpha"))
	if err != nil {
		t.Fatalf("prune reuse group: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want a candidate without the label to be excluded", removed)
	}
	if ps := r.callWith("ps"); !slices.Contains(ps, "label=team=alpha") {
		t.Errorf("ps args = %v, want the label filter", ps)
	}
	if r.callWith("rm") != nil {
		t.Error("label mismatch deleted a reuse-group container")
	}
}
