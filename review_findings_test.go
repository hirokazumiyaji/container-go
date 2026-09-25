package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestDockerNameInspectDoesNotPublishUID(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerRunner{fakeRunner: newTestRunner(), inspectJSON: data}
	cfg := &config{runner: runner, eng: dockerEngine{}, name: "myctr"}
	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectFresh(context.Background())
	if err != nil {
		t.Fatalf("name inspect: %v", err)
	}
	if info.uid != dockerFixtureID {
		t.Fatalf("inspected UID = %q, want fixture UID", info.uid)
	}
	if ctr.uid != "" {
		t.Fatalf("name inspect published UID into handle: %q", ctr.uid)
	}
	if ctr.info != nil {
		t.Fatalf("name inspect cached identity before ownership validation: %+v", ctr.info)
	}
}

func TestDockerOperationsFailClosedWithoutUID(t *testing.T) {
	runner := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	if _, err := ctr.State(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("State error = %v, want ErrGenerationReplaced", err)
	}
	if err := ctr.Stop(context.Background(), nil); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Stop error = %v, want ErrGenerationReplaced", err)
	}
	if err := ctr.Terminate(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Terminate error = %v, want ErrGenerationReplaced", err)
	}
	if _, err := ctr.Logs(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Logs error = %v, want ErrGenerationReplaced", err)
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Exec error = %v, want ErrGenerationReplaced", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unverified Docker handle issued CLI calls: %v", runner.calls)
	}
}

func TestDockerFailedCreateCleanupRequiresExactOwnership(t *testing.T) {
	const creation = "0123456789abcdef"
	runner := &dockerRunner{
		fakeRunner:  newTestRunner(),
		inspectJSON: []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:%q}}}]`, dockerFixtureID, managedLabel, sessionLabel, sessionID())),
	}
	cfg := &config{runner: runner, eng: dockerEngine{}, name: "myctr", creation: creation}
	if err := cleanupFailedCreate(context.Background(), cfg, errors.New("start failed"), errors.New("start failed")); err != nil {
		t.Fatalf("cleanup error = %v, want refusal without generation", err)
	}
	if len(runner.callWith("rm")) != 0 {
		t.Fatal("cleanup deleted a container without an exact creation generation")
	}

	owned := &dockerRunner{
		fakeRunner:  newTestRunner(),
		inspectJSON: []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:%q,%q:%q}}}]`, dockerFixtureID, managedLabel, sessionLabel, sessionID(), creationLabel, creation)),
	}
	cfg.runner = owned
	if err := cleanupFailedCreate(context.Background(), cfg, errors.New("start failed"), errors.New("start failed")); err != nil {
		t.Fatalf("owned cleanup error = %v", err)
	}
	rm := owned.callWith("rm")
	if len(rm) == 0 || rm[len(rm)-1] != dockerFixtureID {
		t.Fatalf("owned cleanup rm = %v, want immutable UID %s", rm, dockerFixtureID)
	}
}

func TestDockerRejectsMacvlanAndIPvlanPublishing(t *testing.T) {
	for _, driver := range []string{"macvlan", "ipvlan"} {
		t.Run(driver, func(t *testing.T) {
			runner := &dockerRunner{
				fakeRunner:         newTestRunner(),
				networkInspectJSON: []byte(fmt.Sprintf(`[{"Name":"private","Driver":%q,"Internal":false,"Options":{}}]`, driver)),
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithNetwork("private"), WithExposedPorts("6379/tcp"),
				withRunner(runner), withEngine(dockerEngine{}))
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), driver) {
				t.Fatalf("Run error = %v, want %s driver rejection", err, driver)
			}
			if len(runner.callWith("run")) != 0 {
				t.Fatal("driver rejection happened after container create")
			}
		})
	}
}

func TestDockerConcreteModeRequiresCurrentNetworkMembership(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), `"bridge": {`, `"other": {`, 1)
	runner := &dockerRunner{
		fakeRunner:       newTestRunner(),
		inspectResponses: [][]byte{[]byte(data), []byte(changed)},
	}
	ctr := runDockerTestContainer(t, runner, WithNetwork("bridge"), WithExposedPorts("6379/tcp"))
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("first endpoint: %v", err)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("changed attachment endpoint error = %v, want ErrNetworkMismatch", err)
	}
}

type reviewReuseNetworkRunner struct {
	*fakeRunner
	serverOS string
	network  string
}

func (r *reviewReuseNetworkRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "version" {
		return []byte(r.serverOS), nil, nil
	}
	if args[0] == "inspect" {
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/shared-network-order","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef"}},"HostConfig":{"NetworkMode":"default"},"NetworkSettings":{"Networks":{%q:{"IPAddress":"172.20.0.2"}}}}]`, dockerFixtureID, managedLabel, reuseLabel, creationLabel, r.network)), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestConcurrentReuseResolvesDefaultNetworkPerCaller(t *testing.T) {
	windowsRunner := &reviewReuseNetworkRunner{fakeRunner: newTestRunner(), serverOS: "windows", network: "nat"}
	linuxRunner := &reviewReuseNetworkRunner{fakeRunner: newTestRunner(), serverOS: "linux", network: "bridge"}
	type result struct {
		ctr *Container
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, tc := range []struct {
		runner *reviewReuseNetworkRunner
	}{
		{windowsRunner},
		{linuxRunner},
	} {
		tc := tc
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared-network-order"), WithReuse(),
				withRunner(tc.runner), withEngine(dockerEngine{}))
			results <- result{ctr: ctr, err: err}
		}()
	}
	wg.Wait()
	close(results)
	got := map[string]bool{}
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent reuse: %v", result.err)
		}
		if result.ctr == nil {
			t.Fatal("concurrent reuse returned a nil handle")
		}
		got[result.ctr.defaultNetwork] = true
	}
	if !got["nat"] || !got["bridge"] {
		t.Fatalf("default network metadata = %v, want caller-specific nat and bridge", got)
	}
}

type reviewAppleEndpointRunner struct {
	*fakeRunner
	mu       sync.Mutex
	inspects int
}

func (r *reviewAppleEndpointRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.inspects++
	creation := r.creations["myctr"]
	if creation == "" {
		creation = "aaaaaaaaaaaaaaaa"
	}
	if r.inspects > 2 {
		creation = "bbbbbbbbbbbbbbbb"
	}
	r.mu.Unlock()
	return []byte(reuseInspectJSONWithCreation("myctr", "running", "redis:7-alpine", creation)), nil, nil
}

func TestDirectIPPublishedEndpointsRequireFreshIdentity(t *testing.T) {
	runner := &reviewAppleEndpointRunner{fakeRunner: newTestRunner()}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPublishedPort("127.0.0.1:16379:6379/tcp"),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if host, err := ctr.Host(context.Background()); err != nil || host != "127.0.0.1" {
		t.Fatalf("initial Host = %q, err = %v", host, err)
	}
	if endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil || endpoint != "127.0.0.1:16379" {
		t.Fatalf("initial Endpoint = %q, err = %v", endpoint, err)
	}
	if _, err := ctr.Host(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("replacement Host error = %v, want ErrGenerationReplaced", err)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("replacement Endpoint error = %v, want ErrGenerationReplaced", err)
	}
	if runner.inspects < 4 {
		t.Fatalf("inspect count = %d, want fresh inspect for each endpoint", runner.inspects)
	}
}

func TestDockerRunRejectsMissingImmutableID(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &runIDRunner{fakeRunner: base, id: "not-an-immutable-id"}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Run error = %v, want ErrGenerationReplaced", err)
	}
	if len(runner.callWith("rm")) != 0 {
		t.Fatal("invalid Docker run ID cleanup fell back to a name delete")
	}
}

type runIDRunner struct {
	*fakeRunner
	id string
}

func (r *runIDRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return []byte(r.id + "\n"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestDeleteStoppedReuseRejectsSameGenerationRunningPeer(t *testing.T) {
	runner := &reviewAppleStateRunner{state: "running"}
	cfg := &config{runner: runner, eng: appleEngine{}, name: "shared"}
	info := &engineInfo{
		state: StateStopped,
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "aaaaaaaaaaaaaaaa",
		},
	}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse: %v", err)
	}
	if runner.deleteCalls != 0 {
		t.Fatal("stopped recycle deleted a generation that became running")
	}
}

type reviewAppleStateRunner struct {
	mu          sync.Mutex
	state       string
	deleteCalls int
}

func (r *reviewAppleStateRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		state := r.state
		r.mu.Unlock()
		return []byte(fmt.Sprintf(`[{"id":"shared","configuration":{"id":"shared","image":{"reference":"redis:7-alpine"},"labels":{%q:"true",%q:"true",%q:"aaaaaaaaaaaaaaaa"}},"status":{"state":%q,"networks":[]}}]`, managedLabel, reuseLabel, creationLabel, state)), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.deleteCalls++
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestCleanupRefusesForeignCreationEvenWithMatchingSession(t *testing.T) {
	runner := &dockerRunner{
		fakeRunner:  newTestRunner(),
		inspectJSON: []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{"managed":"true","session":%q,"creation":"bbbbbbbbbbbbbbbb"}}}]`, dockerFixtureID, sessionID())),
	}
	cfg := &config{runner: runner, eng: dockerEngine{}, name: "myctr", creation: "aaaaaaaaaaaaaaaa"}
	if err := cleanupFailedCreate(context.Background(), cfg, errors.New("failed"), errors.New("failed")); err != nil {
		t.Fatalf("cleanup error = %v", err)
	}
	if len(runner.callWith("rm")) != 0 {
		t.Fatal("cleanup deleted a different creation generation")
	}
}
