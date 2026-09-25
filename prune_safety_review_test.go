package container

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type reviewPruneRunner struct {
	mu      sync.Mutex
	list    []byte
	fresh   []byte
	deleted []string
}

func (r *reviewPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "ls":
		return r.list, nil, nil
	case "inspect":
		return r.fresh, nil, nil
	case "delete", "rm":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func reviewPruneJSON(name, creation, state string) []byte {
	value := []map[string]any{{
		"id": name,
		"configuration": map[string]any{"labels": map[string]string{
			managedLabel:  "true",
			creationLabel: creation,
		}},
		"status": map[string]any{"state": state},
	}}
	data, _ := json.Marshal(value)
	return data
}

func TestReviewApplePruneRejectsReplacement(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	runner := &reviewPruneRunner{
		list:  reviewPruneJSON("review-prune", "aaaaaaaaaaaaaaaa", string(StateStopped)),
		fresh: reviewPruneJSON("review-prune", "bbbbbbbbbbbbbbbb", string(StateStopped)),
	}
	removed, err := pruneWith(context.Background(), runner, appleEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 || len(runner.deleted) != 0 {
		t.Fatalf("replacement was removed: removed=%v deleted=%v", removed, runner.deleted)
	}
}

func TestReviewApplePruneRejectsUnknownRawState(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	runner := &reviewPruneRunner{
		list:  reviewPruneJSON("review-prune-unknown", "aaaaaaaaaaaaaaaa", "future-state"),
		fresh: reviewPruneJSON("review-prune-unknown", "aaaaaaaaaaaaaaaa", "future-state"),
	}
	removed, err := pruneWith(context.Background(), runner, appleEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 || len(runner.deleted) != 0 {
		t.Fatalf("unknown state was removed: removed=%v deleted=%v", removed, runner.deleted)
	}
}

func TestReviewApplePruneDeletesOnlyMatchingGeneration(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	runner := &reviewPruneRunner{
		list:  reviewPruneJSON("review-prune-ok", "aaaaaaaaaaaaaaaa", string(StateStopped)),
		fresh: reviewPruneJSON("review-prune-ok", "aaaaaaaaaaaaaaaa", string(StateStopped)),
	}
	removed, err := pruneWith(context.Background(), runner, appleEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "review-prune-ok" || len(runner.deleted) != 1 {
		t.Fatalf("matching prune failed: removed=%v deleted=%v", removed, runner.deleted)
	}
}
