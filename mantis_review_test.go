package container

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Adoption must not hold the exclusive name lock for the whole attach
// budget: every peer name-addressed operation waits on the same lock with a
// 30s budget, so a container that stays in Created/Stopping longer than
// reuseLockedPollBudget would make a concurrent Terminate or Prune fail.
func TestReviewReusePollLockHoldIsBounded(t *testing.T) {
	if reuseLockedPollBudget >= queryTimeout {
		t.Fatalf("reuseLockedPollBudget = %v must stay under the %v peer budget", reuseLockedPollBudget, queryTimeout)
	}
}

// A stopped container that restarted between the list call and the delete
// must not be force-removed: Prune only targets stopped containers.
func TestReviewImmutablePruneSkipsRestartedContainer(t *testing.T) {
	id := strings.Repeat("a", 64)
	r := &restartedPruneRunner{state: "running", managed: true}
	removed, err := pruneListed(context.Background(), r, dockerEngine{},
		[]string{"ps"}, staticIDParser(id), "prune", "")
	if err != nil {
		t.Fatalf("pruneListed: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want nothing for a running container", removed)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", r.deleteCalls)
	}
}

// The same prune must still remove a stopped managed container, so the
// re-verification is a safety check and not a blanket refusal.
func TestReviewImmutablePruneRemovesStoppedContainer(t *testing.T) {
	id := strings.Repeat("a", 64)
	r := &restartedPruneRunner{state: "exited", managed: true}
	removed, err := pruneListed(context.Background(), r, dockerEngine{},
		[]string{"ps"}, staticIDParser(id), "prune", "")
	if err != nil {
		t.Fatalf("pruneListed: %v", err)
	}
	if len(removed) != 1 || removed[0] != id {
		t.Errorf("removed = %v, want [%s]", removed, id)
	}
	if r.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", r.deleteCalls)
	}
}

// A reuse-group prune force-removes a stopped group member that carries no
// managed label, which is what the group's contract requires.
func TestReviewImmutablePruneReuseGroupRemovesUnmanaged(t *testing.T) {
	id := strings.Repeat("a", 64)
	r := &restartedPruneRunner{state: "exited", managed: false, group: "integration"}
	removed, err := pruneListed(context.Background(), r, dockerEngine{},
		[]string{"ps"}, staticIDParser(id), "prune reuse group integration", "integration")
	if err != nil {
		t.Fatalf("pruneListed: %v", err)
	}
	if len(removed) != 1 {
		t.Errorf("removed = %v, want the unmanaged group member removed", removed)
	}
}

// A container belonging to a different reuse group must not be removed.
func TestReviewImmutablePruneSkipsForeignGroup(t *testing.T) {
	id := strings.Repeat("a", 64)
	r := &restartedPruneRunner{state: "exited", group: "other"}
	removed, err := pruneListed(context.Background(), r, dockerEngine{},
		[]string{"ps"}, staticIDParser(id), "prune reuse group integration", "integration")
	if err != nil {
		t.Fatalf("pruneListed: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want nothing for a foreign group", removed)
	}
}

// staticIDParser returns a single immutable-ID candidate, mirroring the
// Docker list parser which yields IDs only.
func staticIDParser(id string) func([]byte) ([]pruneCandidate, error) {
	return func([]byte) ([]pruneCandidate, error) {
		return []pruneCandidate{{id: id}}, nil
	}
}

// restartedPruneRunner answers a list call with one ID and an inspect with a
// caller-chosen lifecycle state and label set.
type restartedPruneRunner struct {
	state       string
	managed     bool
	group       string
	deleteCalls int
}

func (r *restartedPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps", "ls":
		return []byte(strings.Repeat("a", 64) + "\n"), nil, nil
	case "inspect":
		id := args[len(args)-1]
		labels := map[string]string{
			creationLabel:   "0123456789abcdef",
			reuseGroupLabel: r.group,
		}
		if r.managed {
			labels[managedLabel] = "true"
		}
		payload, err := json.Marshal([]map[string]any{{
			"Id":         id,
			"Name":       "/x",
			"State":      map[string]string{"Status": r.state},
			"Config":     map[string]any{"Labels": labels},
			"HostConfig": map[string]any{},
		}})
		if err != nil {
			return nil, nil, err
		}
		return payload, nil, nil
	case "rm", "delete":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}
