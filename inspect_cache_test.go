package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAppleCreationA = "0123456789abcdef"
	testAppleCreationB = "fedcba9876543210"
)

func appleInspectWithIP(ip string) string {
	network := fmt.Sprintf(`{"ipv4Address": %q, "network": "default"}`, ip)
	return fmt.Sprintf(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":{}},"status":{"state":"running","networks":[%s]}}]`, network)
}

func appleInspectWithIdentity(ip, creation string) string {
	network := fmt.Sprintf(`{"ipv4Address": %q, "network": "default"}`, ip)
	labels := "{}"
	if creation != "" {
		labels = fmt.Sprintf(`{%q:%q}`, creationLabel, creation)
	}
	return fmt.Sprintf(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":%s},"status":{"state":"running","networks":[%s]}}]`, labels, network)
}

func dockerInspectWithPort(port string) string {
	return dockerInspectWithIDAndPort(dockerFixtureID, port)
}

// dynamicClock is a test clock for the short-TTL dynamic inspect cache.
type dynamicClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *dynamicClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *dynamicClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func installDynamicClock(ctr *Container) *dynamicClock {
	clock := &dynamicClock{at: time.Now()}
	ctr.pack().now = clock.Now
	return clock
}

func testAppleContainer(runner *fakeRunner, exposed ...portSpec) *Container {
	return &Container{
		id:       "myctr",
		runner:   runner,
		eng:      appleEngine{},
		creation: testAppleCreationA,
		exposed:  exposed,
	}
}

func testDockerContainer(runner *fakeRunner, exposed ...portSpec) *Container {
	return &Container{
		id:      "myctr",
		runner:  runner,
		eng:     dockerEngine{},
		sync:    &lockPack{uid: dockerFixtureID},
		exposed: exposed,
	}
}

func TestContainerIPRefreshesChangedInspectResponse(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA),
		appleInspectWithIdentity("192.0.2.2/24", testAppleCreationA),
	}
	ctr := testAppleContainer(runner)
	clock := installDynamicClock(ctr)

	first, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("first ContainerIP: %v", err)
	}
	clock.Advance(dynamicInspectTTL)
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
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})

	info, err := ctr.cachedInfo(context.Background())
	if err != nil {
		t.Fatalf("cachedInfo: %v", err)
	}
	if info.uid != dockerFixtureID || info.image != "redis:7-alpine" {
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
	ctr.pack().mu.Lock()
	cached := ctr.pack().info
	ctr.pack().mu.Unlock()
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
		appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA),
		appleInspectWithIdentity("192.0.2.2/24", testAppleCreationB),
	}
	ctr := testAppleContainer(runner)
	clock := installDynamicClock(ctr)

	if _, err := ctr.ContainerIP(context.Background()); err != nil {
		t.Fatalf("first ContainerIP: %v", err)
	}
	clock.Advance(dynamicInspectTTL)
	if _, err := ctr.ContainerIP(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("changed identity error = %v, want ErrGenerationReplaced", err)
	}
}

func TestContainerIPRetriesEmptyInitialInspect(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		appleInspectWithIdentity("", testAppleCreationA),
		appleInspectWithIdentity("192.0.2.3/24", testAppleCreationA),
	}
	ctr := testAppleContainer(runner)

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
		appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA),
		appleInspectWithIdentity("192.0.2.2/24", testAppleCreationA),
	}
	ctr := testAppleContainer(runner, portSpec{port: 6379, proto: "tcp"})
	clock := installDynamicClock(ctr)

	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	clock.Advance(dynamicInspectTTL)
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
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})
	clock := installDynamicClock(ctr)

	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	clock.Advance(dynamicInspectTTL)
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
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})

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

func TestEndpointReusesDynamicInspectWithinTTL(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		dockerInspectWithPort("49153"),
		dockerInspectWithPort("49154"),
	}
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})
	clock := installDynamicClock(ctr)

	first, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	clock.Advance(dynamicInspectTTL / 2)
	second, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("second Endpoint: %v", err)
	}
	if first != "127.0.0.1:49153" || second != first {
		t.Fatalf("Endpoint values = %q then %q, want cached first value", first, second)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1 within TTL", runner.inspectCalls)
	}
}

func TestConcurrentEndpointRefreshDoesNotReuseStaleSnapshot(t *testing.T) {
	// Two cache-miss Endpoint calls must not let an older inspect finish
	// last and stamp a fresh TTL over a newer snapshot. Serialization on
	// dynamicMu coalesces the second caller onto the first refresh.
	base := newTestRunner()
	base.inspectResponses = []string{
		dockerInspectWithPort("49153"),
		dockerInspectWithPort("49154"),
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var inspectN atomic.Int32
	runner := &blockingDynamicInspectRunner{
		fakeRunner: base,
		started:    started,
		release:    release,
		count:      &inspectN,
	}
	ctr := testDockerContainer(base, portSpec{port: 6379, proto: "tcp"})
	ctr.runner = runner

	errCh := make(chan error, 2)
	go func() {
		_, err := ctr.Endpoint(context.Background(), "6379/tcp")
		errCh <- err
	}()
	<-started
	go func() {
		_, err := ctr.Endpoint(context.Background(), "6379/tcp")
		errCh <- err
	}()
	// Second caller waits on dynamicMu while the first inspect is in flight.
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("Endpoint: %v", err)
		}
	}
	if got := inspectN.Load(); got != 1 {
		t.Fatalf("inspect calls = %d, want 1 coalesced refresh", got)
	}
	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("cached Endpoint: %v", err)
	}
	if endpoint != "127.0.0.1:49153" {
		t.Fatalf("Endpoint = %q, want first inspect result", endpoint)
	}
}

type blockingDynamicInspectRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
	count   *atomic.Int32
	once    sync.Once
}

func (r *blockingDynamicInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.count.Add(1)
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestEndpointReinspectsAfterDynamicTTL(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		dockerInspectWithPort("49153"),
		dockerInspectWithPort("49154"),
	}
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})
	clock := installDynamicClock(ctr)

	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	clock.Advance(dynamicInspectTTL)
	second, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("second Endpoint: %v", err)
	}
	if second != "127.0.0.1:49154" {
		t.Fatalf("Endpoint after TTL = %q", second)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2 after TTL", runner.inspectCalls)
	}
}

func TestStateBypassesDynamicInspectTTL(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{
		dockerInspectWithPort("49153"),
		dockerInspectWithPort("49154"),
	}
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})
	_ = installDynamicClock(ctr)

	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if _, err := ctr.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	if runner.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want Endpoint + always-fresh State", runner.inspectCalls)
	}
}

func TestCanceledContextSkipsDynamicCache(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectWithPort("49153")}
	ctr := testDockerContainer(runner, portSpec{port: 6379, proto: "tcp"})
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ctr.Endpoint(ctx, "6379/tcp"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Endpoint error = %v, want context.Canceled", err)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1 (no re-inspect on canceled cache miss path)", runner.inspectCalls)
	}
}

func reviewExplicitAppleContainer(r *fakeRunner) *Container {
	return &Container{
		id:       "myctr",
		runner:   r,
		eng:      appleEngine{},
		creation: testAppleCreationA,
		published: []publishSpec{{
			hostAddr:      "127.0.0.1",
			hostPort:      16379,
			containerPort: 6379,
			proto:         "tcp",
		}},
	}
}

func TestExplicitPublishedHostVerifiesLiveIdentity(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA)}
	ctr := reviewExplicitAppleContainer(runner)

	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("Host = %q", host)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1", runner.inspectCalls)
	}
}

func TestExplicitPublishedEndpointVerifiesLiveIdentity(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA)}
	ctr := reviewExplicitAppleContainer(runner)

	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "127.0.0.1:16379" {
		t.Fatalf("Endpoint = %q", endpoint)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1", runner.inspectCalls)
	}
}

func TestExplicitPublishedMappedPortVerifiesLiveIdentity(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA)}
	ctr := reviewExplicitAppleContainer(runner)

	port, err := ctr.MappedPort(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("MappedPort: %v", err)
	}
	if port != 16379 {
		t.Fatalf("MappedPort = %d", port)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1", runner.inspectCalls)
	}
}

func dockerInspectWithIDAndPort(id, port string) string {
	return fmt.Sprintf(`[{
		"Id": %q,
		"Name": "/myctr",
		"State": {"Status": "running"},
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"HostConfig": {"NetworkMode": "bridge"},
		"NetworkSettings": {
			"IPAddress": "172.17.0.2",
			"Ports": {"6379/tcp": [{"HostIp": "127.0.0.1", "HostPort": %q}]},
			"Networks": {"bridge": {"IPAddress": "172.17.0.2"}}
		}
	}]`, id, port)
}

func TestExplicitPublishedResolutionRejectsUIDMismatch(t *testing.T) {
	uidA := strings.Repeat("a", 64)
	uidB := strings.Repeat("b", 64)
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectWithIDAndPort(uidB, "16379")}
	ctr := &Container{
		id:     "myctr",
		runner: runner,
		eng:    dockerEngine{},
		sync:   &lockPack{uid: uidA},
		published: []publishSpec{{
			hostAddr:      "127.0.0.1",
			hostPort:      16379,
			containerPort: 6379,
			proto:         "tcp",
		}},
	}

	// Inspect targets the handle UID; a response for a different ID is not found.
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("UID mismatch error = %v, want ErrContainerNotFound", err)
	}
}

func TestExplicitPublishedResolutionRejectsCreationMismatch(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{appleInspectWithIdentity("192.0.2.1/24", testAppleCreationB)}
	ctr := reviewExplicitAppleContainer(runner)

	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("creation mismatch error = %v, want ErrGenerationReplaced", err)
	}
}

func TestDirectIPHostInspectsOnce(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA)}
	ctr := testAppleContainer(runner)

	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "192.0.2.1" {
		t.Fatalf("Host = %q", host)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1", runner.inspectCalls)
	}
}

func TestDockerHostVerifiesLiveIdentityWithoutPublishedPort(t *testing.T) {
	uidA := strings.Repeat("a", 64)
	uidB := strings.Repeat("b", 64)
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectWithIDAndPort(uidB, "16379")}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}, sync: &lockPack{uid: uidA}}

	if _, err := ctr.Host(context.Background()); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("UID mismatch error = %v, want ErrContainerNotFound", err)
	}
}

func TestAppleFirstInspectRejectsMissingGeneration(t *testing.T) {
	cases := map[string]string{
		"missing": appleInspectWithIP("192.0.2.1/24"),
		"empty":   appleInspectWithIdentity("192.0.2.1/24", ""),
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			runner := newTestRunner()
			runner.inspectResponses = []string{response}
			ctr := testAppleContainer(runner)

			if _, err := ctr.ContainerIP(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("missing generation error = %v, want ErrGenerationReplaced", err)
			}
		})
	}
}

func TestAppleRejectsMissingOrEmptyGenerationAfterIdentity(t *testing.T) {
	cases := map[string]string{
		"missing": appleInspectWithIP("192.0.2.2/24"),
		"empty":   appleInspectWithIdentity("192.0.2.2/24", ""),
	}
	for name, second := range cases {
		t.Run(name, func(t *testing.T) {
			runner := newTestRunner()
			runner.inspectResponses = []string{
				appleInspectWithIdentity("192.0.2.1/24", testAppleCreationA),
				second,
			}
			ctr := testAppleContainer(runner)
			clock := installDynamicClock(ctr)
			if _, err := ctr.ContainerIP(context.Background()); err != nil {
				t.Fatalf("first ContainerIP: %v", err)
			}
			clock.Advance(dynamicInspectTTL)
			if _, err := ctr.ContainerIP(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("changed generation error = %v, want ErrGenerationReplaced", err)
			}
		})
	}
}

func dockerInspectWithMultipleNetworks() string {
	return fmt.Sprintf(`[{
		"Id": %q,
		"Name": "/myctr",
		"State": {"Status": "running"},
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"HostConfig": {"NetworkMode": "bridge"},
		"NetworkSettings": {
			"IPAddress": "",
			"Ports": {},
			"Networks": {
				"z-network": {"IPAddress": "10.0.0.2"},
				"a-network": {"IPAddress": "10.0.0.1"}
			}
		}
	}]`, dockerFixtureID)
}

func TestDockerNetworkSelectionIsStableAcrossInspects(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectWithMultipleNetworks()}
	ctr := testDockerContainer(runner)

	for i := 0; i < 100; i++ {
		ip, err := ctr.ContainerIP(context.Background())
		if err != nil {
			t.Fatalf("ContainerIP call %d: %v", i, err)
		}
		if ip != "10.0.0.1" {
			t.Fatalf("ContainerIP call %d = %q, want lexicographically first network", i, ip)
		}
	}
}
