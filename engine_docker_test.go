package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func dockerTestConfig(t *testing.T, opts ...Option) *config {
	t.Helper()
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.name = "myctr"
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestDockerRunArgsAutoPublishExposedPorts(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	cfg := dockerTestConfig(t, WithExposedPorts("6379/tcp", "8080"))

	args := dockerEngine{}.runArgs(cfg, "redis:7-alpine", "")
	joined := strings.Join(args, " ")
	if args[0] != "run" || !slices.Contains(args, "--detach") {
		t.Errorf("args = %v", args)
	}
	// Exposed ports are published to random loopback host ports.
	if !strings.Contains(joined, "--publish 127.0.0.1::6379/tcp") {
		t.Errorf("missing auto-publish for 6379: %s", joined)
	}
	if !strings.Contains(joined, "--publish 127.0.0.1::8080/tcp") {
		t.Errorf("missing auto-publish for 8080: %s", joined)
	}
	if args[len(args)-1] != "redis:7-alpine" {
		t.Errorf("image not last: %v", args)
	}
}

func TestDockerRunArgsSkipAutoPublishForExplicitlyPublished(t *testing.T) {
	cfg := dockerTestConfig(t,
		WithExposedPorts("6379/tcp"),
		WithPublishedPort("127.0.0.1:16379:6379"))

	joined := strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--publish 127.0.0.1:16379:6379") {
		t.Errorf("explicit publish missing: %s", joined)
	}
	if strings.Contains(joined, "--publish 127.0.0.1::6379/tcp") {
		t.Errorf("auto-publish must not duplicate explicit publish: %s", joined)
	}
}

func TestDockerRunArgsCarryCommonFlags(t *testing.T) {
	cfg := dockerTestConfig(t,
		WithCPUs(2), WithMemory("512M"), WithUser("nobody"),
		WithWorkingDir("/work"), WithEntrypoint("/entry.sh"),
		WithLabels(map[string]string{"team": "core"}))

	joined := strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", "/tmp/envfile"), " ")
	for _, want := range []string{
		"--name myctr", "--cpus 2", "--memory 512M", "--user nobody",
		"--workdir /work", "--entrypoint /entry.sh", "--env-file /tmp/envfile",
		"--label team=core", "--label " + managedLabel + "=true",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
}

func TestDockerParseInspect(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}

	info, err := dockerEngine{}.parseInspect(data, "myctr")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if info.state != StateRunning {
		t.Errorf("state = %q", info.state)
	}
	if info.labels["com.github.hirokazumiyaji.container-go.session"] != "f00dcafe" {
		t.Errorf("labels = %v", info.labels)
	}
	if info.ip != "172.17.0.2" {
		t.Errorf("ip = %q", info.ip)
	}
	if info.image != "redis:7-alpine" {
		t.Errorf("image = %q", info.image)
	}
	want := boundPort{containerPort: 6379, proto: "tcp", hostAddr: "127.0.0.1", hostPort: 49153}
	if !slices.Contains(info.bound, want) {
		t.Errorf("bound = %+v, want to contain %+v", info.bound, want)
	}
}

func TestParseInspectTargetErrors(t *testing.T) {
	engines := []struct {
		name       string
		eng        engine
		absentData string
	}{
		{name: "apple", eng: appleEngine{}, absentData: `[{"id":"other"}]`},
		{name: "docker", eng: dockerEngine{}, absentData: `[{"Id":"other"}]`},
	}
	cases := []struct {
		name            string
		data            string
		backendData     bool
		wantNotFound    bool
		wantSyntaxError bool
	}{
		{name: "empty", data: `[]`, wantNotFound: true},
		{name: "target absent", backendData: true, wantNotFound: true},
		{name: "ID missing", data: `[{}]`, wantNotFound: true},
		{name: "malformed JSON", data: `{not json`, wantSyntaxError: true},
	}
	for _, backend := range engines {
		for _, tc := range cases {
			t.Run(backend.name+"/"+tc.name, func(t *testing.T) {
				data := tc.data
				if tc.backendData {
					data = backend.absentData
				}
				_, err := backend.eng.parseInspect([]byte(data), "requested")
				if err == nil {
					t.Fatal("parseInspect: want error, got nil")
				}
				if got := errors.Is(err, ErrContainerNotFound); got != tc.wantNotFound {
					t.Errorf("errors.Is(ErrContainerNotFound) = %t, want %t: %v", got, tc.wantNotFound, err)
				}
				var syntaxErr *json.SyntaxError
				if got := errors.As(err, &syntaxErr); got != tc.wantSyntaxError {
					t.Errorf("errors.As(*json.SyntaxError) = %t, want %t: %v", got, tc.wantSyntaxError, err)
				}
			})
		}
	}
}

func TestDockerParseInspectSelectsRequestedID(t *testing.T) {
	otherID := strings.Repeat("a", 64)
	cases := []struct {
		name         string
		target       string
		data         string
		wantUID      string
		wantNotFound bool
	}{
		{
			name:    "exact ID",
			target:  dockerFixtureID,
			data:    `[{"Id":"` + dockerFixtureID + `","State":{"Status":"running"}}]`,
			wantUID: dockerFixtureID,
		},
		{
			name:    "logical name",
			target:  "myctr",
			data:    `[{"Id":"` + dockerFixtureID + `","Name":"/myctr","State":{"Status":"running"}}]`,
			wantUID: dockerFixtureID,
		},
		{
			name:   "logical name with multiple objects",
			target: "myctr",
			data: `[{"Id":"` + otherID + `","Name":"/other","State":{"Status":"exited"}},` +
				`{"Id":"` + dockerFixtureID + `","Name":"/myctr","State":{"Status":"running"}}]`,
			wantUID: dockerFixtureID,
		},
		{
			name:   "exact ID wins over matching name",
			target: dockerFixtureID,
			data: `[{"Id":"` + otherID + `","Name":"/` + dockerFixtureID + `","State":{"Status":"exited"}},` +
				`{"Id":"` + dockerFixtureID + `","State":{"Status":"running"}}]`,
			wantUID: dockerFixtureID,
		},
		{
			name:         "full ID does not fall back to name",
			target:       dockerFixtureID,
			data:         `[{"Id":"` + otherID + `","Name":"/` + dockerFixtureID + `","State":{"Status":"running"}}]`,
			wantNotFound: true,
		},
		{
			name:   "multiple objects",
			target: dockerFixtureID,
			data: `[{"Id":"` + otherID + `","State":{"Status":"exited"}},` +
				`{"Id":"` + dockerFixtureID + `","State":{"Status":"running"}}]`,
			wantUID: dockerFixtureID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := (dockerEngine{}).parseInspect([]byte(tc.data), tc.target)
			if tc.wantNotFound {
				if !errors.Is(err, ErrContainerNotFound) {
					t.Fatalf("error = %v, want ErrContainerNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInspect: %v", err)
			}
			if info.uid != tc.wantUID {
				t.Errorf("uid = %q, want %q", info.uid, tc.wantUID)
			}
			if info.state != StateRunning {
				t.Errorf("state = %q, want %q", info.state, StateRunning)
			}
		})
	}
}

func TestDockerInspectUsesRunIDAndRejectsMismatch(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d)
	d.inspectJSON = []byte(`[{"Id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Name":"/myctr","State":{"Status":"running"}}]`)

	_, err := ctr.State(context.Background())
	inspectCall := d.callWith("inspect")
	if inspectCall == nil {
		t.Fatal("inspect was not called")
	}
	if got := inspectCall[len(inspectCall)-1]; got != dockerFixtureID {
		t.Errorf("inspect target = %q, want Docker run ID %q", got, dockerFixtureID)
	}
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want ErrContainerNotFound", err)
	}
}

type blockingDockerInspectRunner struct {
	*fakeRunner
	data []byte

	mu            sync.Mutex
	inspectTarget []string
	firstStarted  chan struct{}
	secondStarted chan struct{}
}

func (r *blockingDockerInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.inspectTarget = append(r.inspectTarget, args[len(args)-1])
	call := len(r.inspectTarget)
	if call == 1 {
		close(r.firstStarted)
	}
	if call == 2 {
		close(r.secondStarted)
	}
	r.mu.Unlock()

	if call == 1 {
		select {
		case <-r.secondStarted:
		case <-time.After(500 * time.Millisecond):
		}
	}
	return r.data, nil, nil
}

func (r *blockingDockerInspectRunner) targets() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.inspectTarget)
}

func TestDockerConcurrentFirstInspectUsesResolvedID(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingDockerInspectRunner{
		fakeRunner:    newTestRunner(),
		data:          data,
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
	}
	ctr := &Container{
		id:      "myctr",
		runner:  runner,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}

	start := make(chan struct{})
	endpointResult := make(chan error, 1)
	stateResult := make(chan struct {
		state State
		err   error
	}, 1)
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		_, err := ctr.Endpoint(context.Background(), "6379/tcp")
		endpointResult <- err
	}()
	go func() {
		ready.Done()
		<-start
		state, err := ctr.State(context.Background())
		stateResult <- struct {
			state State
			err   error
		}{state: state, err: err}
	}()
	ready.Wait()
	close(start)

	select {
	case <-runner.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first inspect did not start")
	}
	if err := <-endpointResult; err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	result := <-stateResult
	if result.err != nil {
		t.Fatalf("State: %v", result.err)
	}
	if result.state != StateRunning {
		t.Fatalf("State = %q, want %q", result.state, StateRunning)
	}
	if got := ctr.uid; got != dockerFixtureID {
		t.Fatalf("uid = %q, want %q", got, dockerFixtureID)
	}
	wantTargets := []string{"myctr", dockerFixtureID}
	if got := runner.targets(); !slices.Equal(got, wantTargets) {
		t.Errorf("inspect targets = %v, want %v", got, wantTargets)
	}
}

func TestDockerStateMapping(t *testing.T) {
	cases := map[string]State{
		"running":    StateRunning,
		"exited":     StateStopped,
		"dead":       StateStopped,
		"created":    StateCreated,
		"restarting": StateStopping,
		"removing":   StateStopping,
		"paused":     StateUnknown,
	}
	for docker, want := range cases {
		if got := dockerState(docker); got != want {
			t.Errorf("dockerState(%q) = %q, want %q", docker, got, want)
		}
	}
}

func TestDockerLifecycleArgs(t *testing.T) {
	e := dockerEngine{}
	d := 10 * time.Second
	if got := e.stopArgs("myctr", &d); !slices.Equal(got, []string{"stop", "--time", "10", "myctr"}) {
		t.Errorf("stopArgs = %v", got)
	}
	if got := e.deleteArgs("myctr"); !slices.Equal(got, []string{"rm", "--force", "myctr"}) {
		t.Errorf("deleteArgs = %v", got)
	}
	if e.reaperSubcommand() != "rm" {
		t.Errorf("reaperSubcommand = %q", e.reaperSubcommand())
	}
	if got := e.logsArgs("myctr", true); !slices.Equal(got, []string{"logs", "--follow", "myctr"}) {
		t.Errorf("logsArgs = %v", got)
	}
	execArgs := e.execArgs("myctr", &execConfig{user: "u", workdir: "/w"}, "/tmp/env", []string{"id"})
	if !slices.Equal(execArgs, []string{"exec", "--env-file", "/tmp/env", "--user", "u", "--workdir", "/w", "myctr", "id"}) {
		t.Errorf("execArgs = %v", execArgs)
	}
	if got := e.probe().Args; !slices.Equal(got, []string{"version", "--format", "{{.Server.Version}}"}) {
		t.Errorf("probe = %v", got)
	}
}

func TestDockerParseStoppedManaged(t *testing.T) {
	e := dockerEngine{}
	if got := e.listArgs(); !slices.Contains(got, "--filter") {
		t.Errorf("listArgs = %v, want daemon-side filters", got)
	}
	ids, err := e.parseStoppedManaged([]byte("one\ntwo\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"one", "two"}) {
		t.Errorf("ids = %v", ids)
	}
}

// dockerRunner serves docker-shaped responses.
// dockerFixtureID is the Id in testdata/docker_inspect_v29.json, which
// `docker run --detach` also prints on stdout.
const dockerFixtureID = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

type dockerRunner struct {
	*fakeRunner
	inspectJSON []byte
	failInspect bool
}

func (d *dockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	d.calls = append(d.calls, args)
	switch args[0] {
	case "info":
		return []byte("ok"), nil, nil
	case "run":
		return []byte(dockerFixtureID + "\n"), nil, nil
	case "inspect":
		if d.failInspect {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected failure"}
		}
		return d.inspectJSON, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestDockerTerminateDeletesByRunIDWithoutInspect(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d)
	if ctr.uid != dockerFixtureID {
		t.Fatalf("uid = %q, want the ID docker run printed", ctr.uid)
	}
	inspects := len(d.calls)
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	// Delete by immutable ID needs no inspect: exactly one call follows.
	if extra := d.calls[inspects:]; len(extra) != 1 || extra[0][0] != "rm" {
		t.Errorf("Terminate issued %v, want a single rm", extra)
	}
	if rm := d.callWith("rm"); rm == nil || rm[len(rm)-1] != dockerFixtureID {
		t.Errorf("rm = %v, want delete by %s", rm, dockerFixtureID)
	}
}

func TestDockerParseRunID(t *testing.T) {
	e := dockerEngine{}
	if got := e.parseRunID([]byte(dockerFixtureID + "\n")); got != dockerFixtureID {
		t.Errorf("parseRunID = %q", got)
	}
	for _, out := range []string{"", "0f1e2d3c\n", "WARNING: something\n" + dockerFixtureID + "\n"} {
		if got := e.parseRunID([]byte(out)); got != "" {
			t.Errorf("parseRunID(%q) = %q, want empty", out, got)
		}
	}
}

func runDockerTestContainer(t *testing.T, d *dockerRunner, opts ...Option) *Container {
	t.Helper()
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	d.inspectJSON = data
	opts = append([]Option{WithName("myctr"), withRunner(d), withEngine(dockerEngine{})}, opts...)
	ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return ctr
}

func TestDockerEndpointsUseAssignedHostPort(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))

	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "127.0.0.1" {
		t.Errorf("Host = %q", host)
	}
	port, err := ctr.MappedPort(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("MappedPort: %v", err)
	}
	if port != 49153 {
		t.Errorf("MappedPort = %d, want 49153 (daemon-assigned)", port)
	}
	ep, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep != "127.0.0.1:49153" {
		t.Errorf("Endpoint = %q", ep)
	}
}

func TestDockerMappedPortRejectsUndeclaredPort(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))

	if _, err := ctr.MappedPort(context.Background(), "5432/tcp"); err == nil {
		t.Fatal("want error for undeclared port")
	}
}

func TestDockerHostHonorsDockerHostEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	if got := (dockerEngine{}).defaultHost(); got != "10.0.0.5" {
		t.Errorf("defaultHost = %q, want 10.0.0.5", got)
	}

	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	if got := (dockerEngine{}).defaultHost(); got != "127.0.0.1" {
		t.Errorf("defaultHost = %q, want 127.0.0.1", got)
	}
}

func TestDockerRunArgsBindAllInterfacesOnRemoteDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://docker:2375")
	cfg := dockerTestConfig(t, WithExposedPorts("6379/tcp"))
	joined := strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--publish 0.0.0.0::6379/tcp") {
		t.Errorf("remote auto-publish must bind 0.0.0.0: %s", joined)
	}
	if strings.Contains(joined, "127.0.0.1::6379") {
		t.Errorf("remote must not bind loopback: %s", joined)
	}

	t.Setenv("DOCKER_HOST", "")
	cfg = dockerTestConfig(t, WithExposedPorts("6379/tcp"))
	joined = strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--publish 127.0.0.1::6379/tcp") {
		t.Errorf("local auto-publish must bind loopback: %s", joined)
	}
}

func TestDockerEndpointsRewriteLoopbackOnRemoteDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))
	// testdata binds 127.0.0.1:49153 on the daemon; the client must dial
	// the remote host instead.
	ep, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep != "10.0.0.5:49153" {
		t.Errorf("Endpoint = %q, want 10.0.0.5:49153", ep)
	}
}

func TestDockerConnectHostMapping(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	eng := dockerEngine{}
	for _, addr := range []string{"", "0.0.0.0", "::", "127.0.0.1", "127.0.0.2", "::1", "localhost"} {
		if got := dockerConnectHost(addr, eng); got != "10.0.0.5" {
			t.Errorf("dockerConnectHost(%q) = %q, want 10.0.0.5", addr, got)
		}
	}
	if got := dockerConnectHost("192.168.1.10", eng); got != "192.168.1.10" {
		t.Errorf("explicit host must pass through, got %q", got)
	}

	t.Setenv("DOCKER_HOST", "")
	for _, addr := range []string{"", "0.0.0.0", "::"} {
		if got := dockerConnectHost(addr, eng); got != "127.0.0.1" {
			t.Errorf("local unspecified dockerConnectHost(%q) = %q, want 127.0.0.1", addr, got)
		}
	}
	for _, addr := range []string{"127.0.0.1", "::1", "localhost"} {
		if got := dockerConnectHost(addr, eng); got != addr {
			t.Errorf("local explicit dockerConnectHost(%q) = %q, want preserved", addr, got)
		}
	}
}

func TestIsRemoteDockerHostUsesFullLoopbackRange(t *testing.T) {
	for _, host := range []string{"", "unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "tcp://127.0.0.2:2375", "tcp://[::1]:2375"} {
		t.Setenv("DOCKER_HOST", host)
		if isRemoteDockerHost() {
			t.Errorf("DOCKER_HOST=%q: want local (not remote)", host)
		}
	}
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	if !isRemoteDockerHost() {
		t.Error("tcp://10.0.0.5:2375 must be remote")
	}
}

func TestDockerRejectsLoopbackPublishOnRemoteDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	d := &dockerRunner{fakeRunner: newTestRunner()}
	for _, spec := range []string{"127.0.0.1:18080:80", "[::1]:18080:80", "127.0.0.2:18080:80"} {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), withRunner(d), withEngine(dockerEngine{}), WithPublishedPort(spec))
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Errorf("WithPublishedPort(%q) on remote daemon: err = %v, want unreachable rejection", spec, err)
		}
	}
	if len(d.callWith("run")) != 0 {
		t.Errorf("run must not be issued: %v", d.callWith("run"))
	}
	// Unspecified and non-loopback binds stay allowed.
	for _, spec := range []string{"0.0.0.0:18080:80", "10.0.0.5:18080:80", "18080:80"} {
		cfg := dockerTestConfig(t, WithPublishedPort(spec))
		if err := (dockerEngine{}).checkConfig(cfg); err != nil {
			t.Errorf("checkConfig(%q) = %v, want nil", spec, err)
		}
	}

	t.Setenv("DOCKER_HOST", "")
	cfg := dockerTestConfig(t, WithPublishedPort("127.0.0.1:18080:80"))
	if err := (dockerEngine{}).checkConfig(cfg); err != nil {
		t.Errorf("local loopback publish must be allowed: %v", err)
	}
}

func TestDockerRunArgsKeepLoopbackOnLoopbackDOCKERHOST(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.2:2375")
	cfg := dockerTestConfig(t, WithExposedPorts("6379/tcp"))
	joined := strings.Join(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), " ")
	if !strings.Contains(joined, "--publish 127.0.0.1::6379/tcp") {
		t.Errorf("loopback DOCKER_HOST must keep loopback publish: %s", joined)
	}
	if strings.Contains(joined, "0.0.0.0::6379") {
		t.Errorf("loopback DOCKER_HOST must not bind all interfaces: %s", joined)
	}
}

func TestDockerContainerIPComesFromInspect(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d)

	ip, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("ContainerIP: %v", err)
	}
	if ip != "172.17.0.2" {
		t.Errorf("ContainerIP = %q", ip)
	}
}
