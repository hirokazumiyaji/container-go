package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type dynamicEndpointRunner struct {
	*fakeRunner
	responses []string
	inspects  int
}

func (r *dynamicEndpointRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		if r.inspects >= len(r.responses) {
			r.inspects++
			return nil, nil, errors.New("missing endpoint fixture")
		}
		response := r.responses[r.inspects]
		r.inspects++
		return []byte(response), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func dynamicDockerEndpointInspect(uid string, hostPort string) string {
	ports := "{}"
	if hostPort != "" {
		ports = fmt.Sprintf(`{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}`, hostPort)
	}
	return fmt.Sprintf(`[{"Id":%q,"State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{}},"NetworkSettings":{"Ports":%s}}]`, uid, ports)
}

func dynamicAppleEndpointInspect(name, creation, ip string) string {
	return fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine"},"labels":{%q:"true",%q:"true",%q:%q},"publishedPorts":[]},"status":{"state":"running","networks":[{"ipv4Address":%q,"network":"default"}]}}]`, name, name, managedLabel, reuseLabel, creationLabel, creation, ip+"/24")
}

func TestDirectAppleEndpointRefreshesLiveAddress(t *testing.T) {
	const (
		name     = "direct-endpoint"
		creation = "aaaaaaaaaaaaaaaa"
	)
	runner := &dynamicEndpointRunner{
		fakeRunner: newTestRunner(),
		responses: []string{
			dynamicAppleEndpointInspect(name, creation, "192.168.64.3"),
			dynamicAppleEndpointInspect(name, creation, "192.168.64.4"),
		},
	}
	ctr := &Container{
		id:       name,
		runner:   runner,
		eng:      appleEngine{},
		creation: creation,
		exposed:  []portSpec{{port: 6379, proto: "tcp"}},
	}
	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first direct Endpoint: %v", err)
	}
	second, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("second direct Endpoint: %v", err)
	}
	if first != "192.168.64.3:6379" || second != "192.168.64.4:6379" {
		t.Fatalf("direct endpoints = %q then %q, want fresh addresses", first, second)
	}
	if runner.inspects != 2 {
		t.Fatalf("inspect calls = %d, want a fresh inspect for each direct endpoint", runner.inspects)
	}
}

func TestExplicitDockerEndpointRefreshesLiveBinding(t *testing.T) {
	uid := strings.Repeat("a", 64)
	base := newTestRunner()
	base.imagePresent = true
	runner := &dynamicEndpointRunner{
		fakeRunner: base,
		responses: []string{
			dynamicDockerEndpointInspect(uid, "16379"),
			dynamicDockerEndpointInspect(uid, "16379"),
		},
	}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("dynamic-endpoint"),
		WithExposedPorts("6379/tcp"),
		WithPublishedPort("127.0.0.1:16379:6379/tcp"),
		withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	second, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("second Endpoint: %v", err)
	}
	if first != "127.0.0.1:16379" || second != "127.0.0.1:16379" {
		t.Fatalf("endpoints = %q then %q, want the live explicit binding", first, second)
	}
	if runner.inspects != 2 {
		t.Fatalf("inspect calls = %d, want a fresh inspect for each endpoint", runner.inspects)
	}
}

func TestExplicitDockerEndpointRejectsMissingLiveBinding(t *testing.T) {
	uid := strings.Repeat("a", 64)
	base := newTestRunner()
	base.imagePresent = true
	runner := &dynamicEndpointRunner{
		fakeRunner: base,
		responses: []string{
			dynamicDockerEndpointInspect(uid, "16379"),
			dynamicDockerEndpointInspect(uid, ""),
		},
	}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("dynamic-missing-endpoint"),
		WithExposedPorts("6379/tcp"),
		WithPublishedPort("127.0.0.1:16379:6379/tcp"),
		withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("initial Endpoint: %v", err)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("missing live binding error = %v, want ErrPortNotExposed", err)
	}
}
