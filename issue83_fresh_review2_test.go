package container

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

type dockerGroupPruneRaceRunner struct {
	mu            sync.Mutex
	firstInspect  []byte
	secondInspect []byte
	inspectCalls  int
	deletes       []string
}

func (r *dockerGroupPruneRaceRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		r.inspectCalls++
		n := r.inspectCalls
		r.mu.Unlock()
		if n == 1 {
			return r.firstInspect, nil, nil
		}
		return r.secondInspect, nil, nil
	case "rm":
		r.mu.Lock()
		r.deletes = append(r.deletes, args[len(args)-1])
		r.mu.Unlock()
	}
	return nil, nil, nil
}

func dockerGroupPruneInspect(uid, state string, labels map[string]string) []byte {
	data := map[string]any{
		"Id":    uid,
		"Name":  "/group-target",
		"State": map[string]string{"Status": state},
		"Config": map[string]any{
			"Image":  "redis:7-alpine",
			"Labels": labels,
		},
		"NetworkSettings": map[string]any{},
	}
	encoded, err := json.Marshal([]map[string]any{data})
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestDockerPruneReuseGroupRejectsFreshForeignRunningCandidate(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	runner := &dockerGroupPruneRaceRunner{
		firstInspect: dockerGroupPruneInspect(uid, "exited", map[string]string{
			managedLabel:    "true",
			reuseLabel:      "true",
			creationLabel:   "aaaaaaaaaaaaaaaa",
			reuseGroupLabel: "ci",
		}),
		secondInspect: dockerGroupPruneInspect(uid, "running", map[string]string{
			reuseGroupLabel: "ci",
		}),
	}

	removed, err := pruneReuseGroupWith(context.Background(), runner, dockerEngine{}, "ci")
	if err != nil {
		t.Fatalf("pruneReuseGroupWith: %v", err)
	}
	if len(removed) != 0 || len(runner.deletes) != 0 {
		t.Fatalf("fresh foreign/running candidate was removed: removed=%v deletes=%v", removed, runner.deletes)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want list-time and fresh validation", runner.inspectCalls)
	}
}

type lateIPReuseRunner struct {
	*fakeRunner
	name      string
	mu        sync.Mutex
	inspects  int
	lateFinal bool
}

func (r *lateIPReuseRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.inspects++
	n := r.inspects
	name := r.name
	r.mu.Unlock()
	if name == "" {
		name = "late-ip"
	}
	networks := "[]"
	missingIP := n == 1
	if r.lateFinal {
		missingIP = n == 2
	}
	if !missingIP {
		networks = `[{"ipv4Address":"192.168.64.3/24","network":"default"}]`
	}
	return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":{"%s":"true","%s":"true","%s":"aaaaaaaaaaaaaaaa"}},"status":{"state":"running","networks":%s}}]`,
		name, name, managedLabel, reuseLabel, creationLabel, networks)), nil, nil
}

func TestReuseDirectIPWaitsForLateAddress(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &lateIPReuseRunner{fakeRunner: base}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("late-ip"), WithReuse(), WithExposedPorts("6379/tcp"),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint after late IP: %v", err)
	}
	if endpoint != "192.168.64.3:6379" {
		t.Fatalf("Endpoint = %q, want late reported IP", endpoint)
	}
	if runner.inspects < 3 {
		t.Fatalf("inspect calls = %d, want readiness and final re-inspect", runner.inspects)
	}
}

func TestReuseDirectIPRetriesIncompleteFinalInspect(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &lateIPReuseRunner{fakeRunner: base, name: "late-final-ip", lateFinal: true}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("late-final-ip"), WithReuse(), WithExposedPorts("6379/tcp"),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.inspects < 3 {
		t.Fatalf("inspect calls = %d, want incomplete final inspect to be retried", runner.inspects)
	}
}
