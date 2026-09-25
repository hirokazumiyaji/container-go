package container

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func appleInspectWithIP(ip string) string {
	network := fmt.Sprintf(`{"ipv4Address": %q, "network": "default"}`, ip)
	return fmt.Sprintf(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":{}},"status":{"state":"running","networks":[%s]}}]`, network)
}

func appleInspectWithIdentity(ip, creation string) string {
	network := fmt.Sprintf(`{"ipv4Address": %q, "network": "default"}`, ip)
	return fmt.Sprintf(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":{%q:%q}},"status":{"state":"running","networks":[%s]}}]`, creationLabel, creation, network)
}

func dockerInspectWithPort(port string) string {
	return fmt.Sprintf(`[{"Id":"myctr","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{}},"NetworkSettings":{"IPAddress":"172.17.0.2","Ports":{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}}}]`, port)
}

func TestContainerIPRefreshesChangedInspectResponse(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		appleInspectWithIP("192.0.2.1/24"),
		appleInspectWithIP("192.0.2.2/24"),
	}
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}

	first, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("first ContainerIP: %v", err)
	}
	second, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("second ContainerIP: %v", err)
	}
	if first != "192.0.2.1" || second != "192.0.2.2" {
		t.Fatalf("ContainerIP values = %q then %q", first, second)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", runner.inspectCalls)
	}
}

func TestCachedInfoKeepsOnlyImmutableIdentity(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		dockerInspectWithPort("49153"),
		dockerInspectWithPort("49154"),
	}
	ctr := &Container{
		id:      "myctr",
		runner:  runner,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}

	info, err := ctr.cachedInfo(context.Background())
	if err != nil {
		t.Fatalf("cachedInfo: %v", err)
	}
	if info.uid != "myctr" || info.image != "redis:7-alpine" {
		t.Fatalf("cached identity = %+v", info)
	}
	if info.ip != "" || len(info.bound) != 0 || info.state != "" {
		t.Fatalf("cached dynamic fields = %+v", info)
	}

	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "127.0.0.1:49154" {
		t.Fatalf("Endpoint = %q", endpoint)
	}
	ctr.mu.Lock()
	cached := ctr.info
	ctr.mu.Unlock()
	if cached.ip != "" || len(cached.bound) != 0 || cached.state != "" {
		t.Fatalf("identity cache contains dynamic fields: %+v", cached)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", runner.inspectCalls)
	}
}

func TestDynamicInspectRejectsChangedIdentity(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		appleInspectWithIdentity("192.0.2.1/24", "generation-a"),
		appleInspectWithIdentity("192.0.2.2/24", "generation-b"),
	}
	ctr := &Container{
		id:       "myctr",
		runner:   runner,
		eng:      appleEngine{},
		creation: "generation-a",
	}

	if _, err := ctr.ContainerIP(context.Background()); err != nil {
		t.Fatalf("first ContainerIP: %v", err)
	}
	if _, err := ctr.ContainerIP(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("changed identity error = %v, want ErrGenerationReplaced", err)
	}
}

func TestContainerIPRetriesEmptyInitialInspect(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		appleInspectWithIP(""),
		appleInspectWithIP("192.0.2.3/24"),
	}
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}

	if _, err := ctr.ContainerIP(context.Background()); err == nil {
		t.Fatal("first ContainerIP succeeded without an IP")
	}
	ip, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("retry ContainerIP: %v", err)
	}
	if ip != "192.0.2.3" {
		t.Fatalf("retry ContainerIP = %q", ip)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", runner.inspectCalls)
	}
}

func TestAppleEndpointRefreshesChangedInspectResponse(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		appleInspectWithIP("192.0.2.1/24"),
		appleInspectWithIP("192.0.2.2/24"),
	}
	ctr := &Container{
		id:      "myctr",
		runner:  runner,
		eng:     appleEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}

	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	second, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("second Endpoint: %v", err)
	}
	if first != "192.0.2.1:6379" || second != "192.0.2.2:6379" {
		t.Fatalf("Endpoint values = %q then %q", first, second)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", runner.inspectCalls)
	}
}

func TestEndpointRefreshesChangedInspectResponse(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		dockerInspectWithPort("49153"),
		dockerInspectWithPort("49154"),
	}
	ctr := &Container{
		id:      "myctr",
		runner:  runner,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}

	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	second, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("second Endpoint: %v", err)
	}
	if first != "127.0.0.1:49153" || second != "127.0.0.1:49154" {
		t.Fatalf("Endpoint values = %q then %q", first, second)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", runner.inspectCalls)
	}
}

func TestEndpointRetriesEmptyInitialBinding(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		dockerInspectWithPort(""),
		dockerInspectWithPort("49155"),
	}
	ctr := &Container{
		id:      "myctr",
		runner:  runner,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}

	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err == nil {
		t.Fatal("first Endpoint succeeded without a host binding")
	}
	ep, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("retry Endpoint: %v", err)
	}
	if ep != "127.0.0.1:49155" {
		t.Fatalf("retry Endpoint = %q", ep)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", runner.inspectCalls)
	}
}
