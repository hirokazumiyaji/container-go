package container

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type freshDockerIdentityRunner struct {
	mu       sync.Mutex
	inspects int
}

func (r *freshDockerIdentityRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return nil, nil, nil
	}
	r.mu.Lock()
	r.inspects++
	r.mu.Unlock()
	return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Labels":{}},"NetworkSettings":{}}]`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")), nil, nil
}

func TestFreshReviewNameLookupDoesNotPublishDockerUID(t *testing.T) {
	runner := &freshDockerIdentityRunner{}
	ctr := &Container{id: "logical-name", runner: runner, eng: dockerEngine{}, nameInspect: true}
	if _, err := ctr.inspectFresh(context.Background()); err != nil {
		t.Fatalf("inspectFresh: %v", err)
	}
	if got := ctr.immutableID(); got != "" {
		t.Fatalf("name lookup published UID %q", got)
	}
	if got := ctr.operationTarget(); got != "logical-name" {
		t.Fatalf("name lookup operation target = %q", got)
	}
	if _, err := ctr.verifiedOperationTarget(); err == nil {
		t.Fatal("name lookup became an operational Docker target")
	}
}

func TestFreshReviewDockerHandleNeverFallsBackToName(t *testing.T) {
	runner := &freshDockerIdentityRunner{}
	ctr := &Container{id: "logical-name", runner: runner, eng: dockerEngine{}}
	if _, err := ctr.State(context.Background()); err == nil {
		t.Fatal("Docker handle without an immutable UID inspected by name")
	}
	runner.mu.Lock()
	inspects := runner.inspects
	runner.mu.Unlock()
	if inspects != 0 {
		t.Fatalf("unsafe name inspect calls = %d", inspects)
	}
	if err := ctr.Terminate(context.Background()); err == nil {
		t.Fatal("Docker handle without an immutable UID fell back to name delete")
	}
}
