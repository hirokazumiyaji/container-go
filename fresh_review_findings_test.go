package container

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type freshMetadataRunner struct {
	mu       sync.Mutex
	inspects int
}

func (r *freshMetadataRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return nil, nil, nil
	}
	r.mu.Lock()
	r.inspects++
	n := r.inspects
	r.mu.Unlock()
	if n == 1 {
		return []byte(`[{"id":"meta","configuration":{"id":"meta","image":{"reference":"redis:7-alpine"},"labels":{"` + creationLabel + `":"0123456789abcdef"}},"status":{"state":"running","networks":[]}}]`), nil, nil
	}
	return []byte(`[{"id":"meta","configuration":{"id":"meta","image":{"reference":"redis:7-alpine"},"labels":{"` + creationLabel + `":"0123456789abcdef"}},"status":{"state":"running","networks":[{"ipv4Address":"192.0.2.10/24","network":"default"}]}}]`), nil, nil
}

func TestFreshReviewRefreshesIncompleteEndpointMetadata(t *testing.T) {
	runner := &freshMetadataRunner{}
	ctr := &Container{id: "meta", creation: "0123456789abcdef", runner: runner, eng: appleEngine{}, exposed: []portSpec{{port: 6379, proto: "tcp"}}}
	if _, err := ctr.ContainerIP(context.Background()); err == nil {
		t.Fatal("first incomplete inspect unexpectedly had an IP")
	}
	if got, err := ctr.ContainerIP(context.Background()); err != nil || got != "192.0.2.10" {
		t.Fatalf("second ContainerIP = %q, %v; want refreshed IP", got, err)
	}
}

type freshDockerPortRunner struct {
	mu       sync.Mutex
	inspects int
}

func (r *freshDockerPortRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return nil, nil, nil
	}
	r.mu.Lock()
	r.inspects++
	n := r.inspects
	r.mu.Unlock()
	id := strings.Repeat("b", 64)
	if n == 1 {
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Labels":{}},"NetworkSettings":{"Ports":{}}}]`, id)), nil, nil
	}
	return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Labels":{}},"NetworkSettings":{"Ports":{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}}]`, id)), nil, nil
}

func TestFreshReviewRefreshesIncompletePublishedPortMetadata(t *testing.T) {
	runner := &freshDockerPortRunner{}
	ctr := &Container{
		id:      strings.Repeat("b", 64),
		uid:     strings.Repeat("b", 64),
		runner:  runner,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}
	if _, err := ctr.Endpoint(context.Background(), "6379"); err == nil {
		t.Fatal("first incomplete published-port inspect unexpectedly succeeded")
	}
	got, err := ctr.Endpoint(context.Background(), "6379")
	if err != nil || got != "127.0.0.1:49153" {
		t.Fatalf("second Endpoint = %q, %v; want refreshed binding", got, err)
	}
}

type freshDockerPruneRunner struct {
	mu      sync.Mutex
	deletes []string
}

func (r *freshDockerPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		id := strings.Repeat("a", 64)
		return []byte(id + "\n"), nil, nil
	case "inspect":
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Labels":{%q:"true"}},"NetworkSettings":{}}]`, strings.Repeat("a", 64), managedLabel)), nil, nil
	case "rm":
		r.mu.Lock()
		r.deletes = append(r.deletes, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestFreshReviewDockerPruneDoesNotForceDeleteRunningCandidate(t *testing.T) {
	runner := &freshDockerPruneRunner{}
	removed, err := pruneWith(context.Background(), runner, dockerEngine{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 0 || len(runner.deletes) != 0 {
		t.Fatalf("running candidate was force-deleted: removed=%v deletes=%v", removed, runner.deletes)
	}
}

type freshDockerGroupRunner struct {
	mu      sync.Mutex
	uid     string
	deleted []string
}

func (r *freshDockerGroupRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte(r.uid + "\n"), nil, nil
	case "inspect":
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/fresh-group","State":{"Status":"running"},"Config":{"Labels":{%q:"true",%q:"true",%q:"integration",%q:"0123456789abcdef"}},"NetworkSettings":{}}]`, r.uid, managedLabel, reuseLabel, reuseGroupLabel, creationLabel)), nil, nil
	case "rm":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestFreshReviewDockerPruneReuseGroupAllowsRunningMember(t *testing.T) {
	uid := strings.Repeat("c", 64)
	runner := &freshDockerGroupRunner{uid: uid}
	removed, err := pruneReuseGroupWith(context.Background(), runner, dockerEngine{}, "integration")
	if err != nil {
		t.Fatalf("prune reuse group: %v", err)
	}
	if len(removed) != 1 || removed[0] != uid || len(runner.deleted) != 1 || runner.deleted[0] != uid {
		t.Fatalf("group prune removed=%v deleted=%v, want running member %s", removed, runner.deleted, uid)
	}
}

type freshReuseAttachRunner struct {
	mu       sync.Mutex
	inspects int
}

func (r *freshReuseAttachRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
	case "inspect":
		r.mu.Lock()
		r.inspects++
		n := r.inspects
		r.mu.Unlock()
		if n == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "temporary inspect failure"}
		}
		return []byte(`[{"id":"fresh-reuse","configuration":{"id":"fresh-reuse","image":{"reference":"redis:7-alpine"},"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.reuse":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"running","networks":[]}}]`), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestFreshReviewReuseAttachRetriesTransientInspectError(t *testing.T) {
	oldTimeout, oldPoll := reuseAttachTimeout, reusePollInterval
	reuseAttachTimeout = 300 * time.Millisecond
	reusePollInterval = time.Millisecond
	defer func() { reuseAttachTimeout, reusePollInterval = oldTimeout, oldPoll }()
	runner := &freshReuseAttachRunner{}
	ctr, err := Run(context.Background(), "redis:7-alpine", WithName("fresh-reuse"), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil {
		t.Fatal("Run returned nil container")
	}
}
