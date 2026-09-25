package container

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
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
	if args[0] == "ls" || args[0] == "inspect" {
		l.calls = append(l.calls, args)
		return []byte(l.lsJSON), nil, nil
	}
	return l.fakeRunner.Run(ctx, args...)
}

const pruneLsJSON = `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"aaaaaaaaaaaaaaaa"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-running","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true"}},"status":{"state":"running","networks":[]}},
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

func TestDockerPruneIDListRevalidatesOwnershipAndGroup(t *testing.T) {
	uid := strings.Repeat("a", 64)
	candidates, err := (dockerEngine{}).parseReuseGroupIDs([]byte(uid+"\n"), "group-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].reuseGroup != "group-a" {
		t.Fatalf("candidates = %+v", candidates)
	}
	fresh := &engineInfo{
		state: StateStopped,
		labels: map[string]string{
			managedLabel:    "true",
			reuseLabel:      "true",
			reuseGroupLabel: "group-a",
			creationLabel:   "0123456789abcdef",
		},
		uid: uid,
	}
	if !pruneCandidateStillCurrent(candidates[0], fresh, "group-a") {
		t.Fatal("matching Docker ID candidate was rejected")
	}
	fresh.state = StateRunning
	if !pruneCandidateStillCurrent(candidates[0], fresh, "group-a") {
		t.Fatal("running Docker reuse-group candidate was rejected")
	}
	if pruneCandidateStillCurrent(candidates[0], fresh, "") {
		t.Fatal("running Docker candidate was accepted by ordinary Prune")
	}
}

func TestApplePruneRequiresListTimeGeneration(t *testing.T) {
	r := &lsRunner{fakeRunner: newTestRunner(), lsJSON: pruneLsJSON}
	removed, err := pruneNamedCandidate(context.Background(), r, appleEngine{}, pruneCandidate{id: "managed-stopped", state: StateStopped, managed: true}, "prune", "")
	if err != nil {
		t.Fatal(err)
	}
	if removed || len(r.calls) != 0 {
		t.Fatalf("removed=%v calls=%v; missing list generation must fail before inspect/delete", removed, r.calls)
	}
}

func TestAppleReuseGroupRequiresListTimeGeneration(t *testing.T) {
	r := &lsRunner{fakeRunner: newTestRunner(), lsJSON: pruneLsJSON}
	removed, err := pruneListed(context.Background(), r, appleEngine{}, []string{"list"}, func([]byte) ([]pruneCandidate, error) {
		return []pruneCandidate{{id: "managed-stopped", state: StateStopped, managed: true, reuseGroup: "ci"}}, nil
	}, "prune reuse group ci", "ci")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none without list-time generation", removed)
	}
	for _, call := range r.calls {
		if len(call) > 0 && (call[0] == "inspect" || call[0] == "delete") {
			t.Fatalf("missing list generation reached name operation: %v", call)
		}
	}
}

type dockerRunningGroupRunner struct {
	uid     string
	deleted []string
}

func (r *dockerRunningGroupRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte(r.uid + "\n"), nil, nil
	case "inspect":
		return []byte(fmt.Sprintf(`[{"Id":%q,"Created":"2026-08-19T01:23:45.678901234Z","Name":"/group-member","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:%q,%q:%q}}}]`, r.uid, managedLabel, reuseLabel, creationLabel, "0123456789abcdef", reuseGroupLabel, "ci")), nil, nil
	case "rm":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestDockerPruneReuseGroupRemovesRunningMember(t *testing.T) {
	uid := strings.Repeat("d", 64)
	r := &dockerRunningGroupRunner{uid: uid}
	removed, err := pruneReuseGroupWith(context.Background(), r, dockerEngine{}, "ci")
	if err != nil {
		t.Fatalf("PruneReuseGroup: %v", err)
	}
	if !slices.Equal(removed, []string{uid}) {
		t.Fatalf("removed = %v, want [%s]", removed, uid)
	}
	if !slices.Equal(r.deleted, []string{uid}) {
		t.Fatalf("deleted = %v, want [%s]", r.deleted, uid)
	}
}

func TestSessionLabelValueIsValid(t *testing.T) {
	id := sessionID()
	if len(id) != 16 || strings.ToLower(id) != id {
		t.Errorf("sessionID = %q, want 16 lowercase hex chars", id)
	}
}
