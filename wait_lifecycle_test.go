package container

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
)

type inspectSequenceRunner struct {
	mu       sync.Mutex
	steps    []inspectStep
	inspects int
	image    bool
}

type inspectStep struct {
	data string
	err  error
}

func newInspectSequenceRunner(steps []inspectStep) *inspectSequenceRunner {
	return &inspectSequenceRunner{steps: steps, image: true}
}

func (r *inspectSequenceRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "image":
		if len(args) > 1 && args[1] == "inspect" && r.image {
			return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
		}
		return nil, nil, nil
	case "run":
		return []byte("myctr\n"), nil, nil
	case "inspect":
		i := r.inspects
		r.inspects++
		if i < len(r.steps) {
			step := r.steps[i]
			return []byte(step.data), nil, step.err
		}
		if len(r.steps) == 0 {
			return nil, nil, errors.New("unexpected inspect")
		}
		step := r.steps[len(r.steps)-1]
		return []byte(step.data), nil, step.err
	default:
		return nil, nil, nil
	}
}

func appleInspectWithNetwork(id, state, address string) string {
	network := "null"
	if address != "" {
		network = fmt.Sprintf(`[{"ipv4Address":%q,"network":"default"}]`, address)
	}
	return fmt.Sprintf(`[{
		"id": %q,
		"configuration": {
			"image": {"reference": "redis:7-alpine"},
			"labels": {"com.github.hirokazumiyaji.container-go": "true"},
			"publishedPorts": []
		},
		"status": {"state": %q, "networks": %s}
	}]`, id, state, network)
}

func TestEndpointRefreshesIncompleteCreatedInspect(t *testing.T) {
	created := appleInspectWithNetwork("myctr", "created", "")
	running := appleInspectWithNetwork("myctr", "running", "192.168.64.3/24")
	runner := newInspectSequenceRunner([]inspectStep{{data: created}, {data: running}})
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}, exposed: []portSpec{{port: 6379, proto: "tcp"}}}

	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err == nil {
		t.Fatal("first Endpoint should report that Created has no IP yet")
	}
	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint after Created -> Running: %v", err)
	}
	if endpoint != "192.168.64.3:6379" {
		t.Fatalf("Endpoint = %q", endpoint)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("cached complete Endpoint: %v", err)
	}
	runner.mu.Lock()
	inspects := runner.inspects
	runner.mu.Unlock()
	if inspects != 2 {
		t.Fatalf("inspect calls = %d, want incomplete then complete then cached", inspects)
	}
}

func dockerInspectWithBinding(id, state, address string, hostPort int) string {
	ports := "null"
	if hostPort > 0 {
		ports = fmt.Sprintf(`{"%s/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}]}`, "6379", hostPort)
	}
	return fmt.Sprintf(`[{
		"Id": %q,
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"State": {"Status": %q},
		"NetworkSettings": {"IPAddress": %q, "Ports": %s}
	}]`, id, state, address, ports)
}

func TestEndpointRefreshesIncompleteDockerBinding(t *testing.T) {
	created := dockerInspectWithBinding("myctr", "created", "", 0)
	running := dockerInspectWithBinding("myctr", "running", "172.17.0.2", 49153)
	runner := newInspectSequenceRunner([]inspectStep{{data: created}, {data: running}})
	ctr := &Container{id: "myctr", uid: dockerFixtureID, runner: runner, eng: dockerEngine{}, exposed: []portSpec{{port: 6379, proto: "tcp"}}}

	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err == nil {
		t.Fatal("first Endpoint should report that Created has no host binding yet")
	}
	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint after Created -> Running: %v", err)
	}
	if endpoint != "127.0.0.1:49153" {
		t.Fatalf("Endpoint = %q", endpoint)
	}
}

func TestForHTTPRefreshesCreatedEndpointAndRecoversFromInspectError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	created := appleInspectWithNetwork("myctr", "created", "")
	running := appleInspectWithNetwork("myctr", "running", u.Hostname()+"/32")
	transient := errors.New("temporary inspect failure")
	runner := newInspectSequenceRunner([]inspectStep{
		{data: created},  // initial state probe
		{data: created},  // first endpoint resolution is incomplete
		{err: transient}, // state inspection fails transiently
		{data: created},  // endpoint retry still must not cache incomplete data
		{err: transient}, // another transient inspect failure
		{data: running},  // endpoint retry observes Running and can resolve
	})

	_, err = Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithExposedPorts(u.Port()),
		WithWaitStrategy(wait.ForHTTP("/ready").
			WithPort(u.Port()).
			WithStartupTimeout(2*time.Second).
			WithPollInterval(10*time.Millisecond)),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	runner.mu.Lock()
	inspects := runner.inspects
	runner.mu.Unlock()
	if inspects < 6 {
		t.Fatalf("inspect calls = %d, want Created/error/Running recovery sequence", inspects)
	}
}

func TestContainerStateCanonicalizesUnknownAppleState(t *testing.T) {
	runner := newInspectSequenceRunner([]inspectStep{{
		data: appleInspectWithNetwork("myctr", "future-state", "192.168.64.3/24"),
	}})
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}

	state, err := ctr.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != StateUnknown {
		t.Fatalf("State = %q, want %q", state, StateUnknown)
	}
}

type dockerStateRunner struct {
	*fakeRunner
	status string
}

func (r *dockerStateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "image" {
		return r.fakeRunner.Run(ctx, args...)
	}
	if args[0] != "inspect" {
		return nil, nil, nil
	}
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()
	return []byte(fmt.Sprintf(`[{
		"Id": "myctr",
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"State": {"Status": %q},
		"NetworkSettings": {"IPAddress": "172.17.0.2", "Ports": {}}
	}]`, r.status)), nil, nil
}

func TestReuseFailsFastForPausedAndStoppingContainers(t *testing.T) {
	oldTimeout := reuseAttachTimeout
	oldInterval := reusePollInterval
	reuseAttachTimeout = 250 * time.Millisecond
	reusePollInterval = time.Millisecond
	t.Cleanup(func() {
		reuseAttachTimeout = oldTimeout
		reusePollInterval = oldInterval
	})

	for _, tc := range []struct {
		name   string
		status string
		want   string
		docker bool
	}{
		{name: "paused", status: "paused", want: "paused", docker: true},
		{name: "stopping", status: "stopping", want: "stopping"},
		{name: "removing", status: "removing", want: "stopping", docker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			var err error
			if tc.docker {
				runner := &dockerStateRunner{fakeRunner: newTestRunner(), status: tc.status}
				runner.imagePresent = true
				_, err = Run(context.Background(), "redis:7-alpine",
					WithName("myctr"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
			} else {
				runner := &attachRunner{fakeRunner: newTestRunner(), state: tc.status}
				runner.imagePresent = true
				_, err = Run(context.Background(), "redis:7-alpine",
					WithName("myctr"), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s diagnostic", err, tc.want)
			}
			if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
				t.Fatalf("reuse %s took %v; want fail-fast", tc.name, elapsed)
			}
		})
	}
}
