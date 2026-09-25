package container

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

type dockerPruneEntry struct {
	uid      string
	state    string
	managed  bool
	reuse    bool
	creation string
}

type dockerPruneFreshRunner struct {
	mu          sync.Mutex
	entries     []dockerPruneEntry
	responses   map[string][]dockerPruneEntry
	inspectCall map[string]int
	deletes     []string
}

func (r *dockerPruneFreshRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		var ids []string
		for _, entry := range r.entries {
			if !entry.managed || !slices.Contains(args, "label="+managedLabel+"=true") {
				continue
			}
			if slices.Contains(args, "status="+entry.state) {
				ids = append(ids, entry.uid)
			}
		}
		return []byte(strings.Join(ids, "\n") + "\n"), nil, nil
	case "inspect":
		target := args[len(args)-1]
		r.mu.Lock()
		if r.inspectCall == nil {
			r.inspectCall = make(map[string]int)
		}
		n := r.inspectCall[target]
		r.inspectCall[target]++
		responses := r.responses[target]
		r.mu.Unlock()
		if len(responses) == 0 {
			return nil, nil, fmt.Errorf("no fixture for %s", target)
		}
		if n >= len(responses) {
			n = len(responses) - 1
		}
		return dockerPruneInspectJSON(responses[n]), nil, nil
	case "rm":
		r.mu.Lock()
		r.deletes = append(r.deletes, args[len(args)-1])
		r.mu.Unlock()
	}
	return nil, nil, nil
}

func dockerPruneInspectJSON(entry dockerPruneEntry) []byte {
	labels := fmt.Sprintf(`%q:"true",%q:%q`, managedLabel, creationLabel, entry.creation)
	if entry.reuse {
		labels += fmt.Sprintf(`,%q:"true"`, reuseLabel)
	}
	if !entry.managed {
		labels = fmt.Sprintf(`%q:%q`, creationLabel, entry.creation)
	}
	return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/prune","State":{"Status":%q},"Config":{"Image":"redis:7-alpine","Labels":{%s}},"NetworkSettings":{}}]`, entry.uid, entry.state, labels))
}

func TestDockerPruneUsesFullIDsAndDeadStoppedState(t *testing.T) {
	creation := "aaaaaaaaaaaaaaaa"
	exited := dockerPruneEntry{uid: strings.Repeat("a", 64), state: "exited", managed: true, creation: creation}
	dead := dockerPruneEntry{uid: strings.Repeat("b", 64), state: "dead", managed: true, creation: creation}
	running := dockerPruneEntry{uid: strings.Repeat("c", 64), state: "running", managed: true, creation: creation}
	created := dockerPruneEntry{uid: strings.Repeat("d", 64), state: "created", managed: true, creation: creation}
	foreign := dockerPruneEntry{uid: strings.Repeat("e", 64), state: "exited", creation: creation}
	runner := &dockerPruneFreshRunner{
		entries: []dockerPruneEntry{exited, dead, running, created, foreign},
		responses: map[string][]dockerPruneEntry{
			exited.uid: {exited, exited},
			dead.uid:   {dead, dead},
		},
	}
	removed, err := pruneWith(context.Background(), runner, dockerEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if want := []string{exited.uid, dead.uid}; !slices.Equal(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	if want := []string{exited.uid, dead.uid}; !slices.Equal(runner.deletes, want) {
		t.Fatalf("deletes = %v, want %v", runner.deletes, want)
	}
}

func TestDockerPruneSkipsFreshRunningReplacement(t *testing.T) {
	creation := "aaaaaaaaaaaaaaaa"
	uid := strings.Repeat("f", 64)
	listed := dockerPruneEntry{uid: uid, state: "exited", managed: true, creation: creation}
	fresh := dockerPruneEntry{uid: uid, state: "running", managed: true, creation: creation}
	runner := &dockerPruneFreshRunner{
		entries:   []dockerPruneEntry{listed},
		responses: map[string][]dockerPruneEntry{uid: {listed, fresh}},
	}
	removed, err := pruneWith(context.Background(), runner, dockerEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 0 || len(runner.deletes) != 0 {
		t.Fatalf("running replacement was removed: removed=%v deletes=%v", removed, runner.deletes)
	}
}
