package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
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

func TestDockerParseInspectRejectsMalformedIDForMatchingName(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`[{"Id":"","Name":"/myctr"}]`),
		[]byte(`[{"Id":"not-a-container-id","Name":"/myctr"}]`),
	} {
		_, err := (dockerEngine{}).parseInspect(data, "myctr")
		if err == nil {
			t.Fatalf("parseInspect(%s) accepted a malformed matching object", data)
		}
		if errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("parseInspect(%s) error = %v, want a protocol error rather than not-found", data, err)
		}
		if !strings.Contains(err.Error(), "invalid container ID") {
			t.Fatalf("parseInspect(%s) error = %v, want invalid container ID", data, err)
		}
	}
}

func TestDockerParseInspectRejectsMalformedObjectWhenTargetIsAbsent(t *testing.T) {
	data := []byte(`[{"Id":"not-a-container-id","Name":"/other"}]`)

	_, err := (dockerEngine{}).parseInspect(data, "myctr")
	if err == nil {
		t.Fatal("parseInspect accepted malformed non-empty output as target absence")
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("parseInspect error = %v, want a protocol error rather than not-found", err)
	}
}

func TestDockerParseInspectDoesNotFallbackFromImmutableIDToName(t *testing.T) {
	const target = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const other = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	data := []byte(`[{"Id":"` + other + `","Name":"/` + target + `"}]`)

	_, err := (dockerEngine{}).parseInspect(data, target)
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("parseInspect error = %v, want target-not-found", err)
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

func TestDockerListArgsUseImmutableIDs(t *testing.T) {
	e := dockerEngine{}
	wantStopped := []string{
		"ps", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=" + managedLabel + "=true",
		"--filter", "status=exited",
	}
	if got := e.listArgs(); !slices.Equal(got, wantStopped) {
		t.Errorf("listArgs = %v, want %v", got, wantStopped)
	}
	wantReuse := []string{
		"ps", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=" + reuseGroupLabel + "=integration",
	}
	if got := e.listReuseGroupArgs("integration"); !slices.Equal(got, wantReuse) {
		t.Errorf("listReuseGroupArgs = %v, want %v", got, wantReuse)
	}
}

func TestDockerParsesOnlyImmutableListIDs(t *testing.T) {
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	candidates, err := (dockerEngine{}).parseStoppedManaged([]byte(first + "\n" + second + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].id != first || candidates[1].id != second {
		t.Errorf("candidates = %+v", candidates)
	}
	candidates, err = (dockerEngine{}).parseReuseGroupIDs([]byte(first+"\n"), "integration")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].id != first {
		t.Errorf("reuse candidates = %+v", candidates)
	}
	if _, err := (dockerEngine{}).parseStoppedManaged([]byte("same-name-replacement\n")); err == nil {
		t.Fatal("parseStoppedManaged accepted a name instead of an immutable ID")
	}
}

type dockerPruneListRunner struct {
	*fakeRunner
	listID string
}

func (d *dockerPruneListRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		return []byte(d.listID + "\n"), nil, nil
	}
	return d.fakeRunner.Run(ctx, args...)
}

func TestDockerPruneDeletesListedImmutableID(t *testing.T) {
	id := strings.Repeat("c", 64)
	tests := []struct {
		name  string
		prune func(context.Context, *dockerPruneListRunner) ([]string, error)
	}{
		{
			name: "Prune",
			prune: func(ctx context.Context, r *dockerPruneListRunner) ([]string, error) {
				return pruneWith(ctx, r, dockerEngine{})
			},
		},
		{
			name: "PruneReuseGroup",
			prune: func(ctx context.Context, r *dockerPruneListRunner) ([]string, error) {
				return pruneReuseGroupWith(ctx, r, dockerEngine{}, "integration")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &dockerPruneListRunner{fakeRunner: newTestRunner(), listID: id}
			removed, err := tc.prune(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(removed, []string{id}) {
				t.Fatalf("removed = %v, want [%s]", removed, id)
			}
			rm := r.callWith("rm")
			if rm == nil || !slices.Equal(rm, []string{"rm", "--force", id}) {
				t.Fatalf("rm = %v, want immutable ID %s", rm, id)
			}
			if r.callWith("inspect") != nil {
				t.Fatal("Docker prune inspected a name-addressed candidate")
			}
		})
	}
}

// dockerRunner serves docker-shaped responses.
// dockerFixtureID is the Id in testdata/docker_inspect_v29.json, which
// `docker run --detach` also prints on stdout.
const dockerFixtureID = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

type dockerRunner struct {
	*fakeRunner
	inspectJSON     []byte
	failInspect     bool
	bindRunIdentity bool
	creation        string
}

func (d *dockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	d.calls = append(d.calls, args)
	switch args[0] {
	case "info":
		return []byte("ok"), nil, nil
	case "run":
		for _, arg := range args {
			if creation, ok := strings.CutPrefix(arg, creationLabel+"="); ok {
				d.creation = creation
			}
		}
		return []byte(dockerFixtureID + "\n"), nil, nil
	case "inspect":
		if d.failInspect {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected failure"}
		}
		if !d.bindRunIdentity || d.creation == "" {
			return d.inspectJSON, nil, nil
		}
		var containers []map[string]any
		if err := json.Unmarshal(d.inspectJSON, &containers); err != nil {
			return nil, nil, err
		}
		for _, container := range containers {
			config, _ := container["Config"].(map[string]any)
			labels, _ := config["Labels"].(map[string]any)
			if labels == nil {
				labels = make(map[string]any)
				config["Labels"] = labels
			}
			labels[creationLabel] = d.creation
			labels[sessionLabel] = sessionID()
			labels[managedLabel] = "true"
		}
		data, err := json.Marshal(containers)
		return data, nil, err
	default:
		return nil, nil, nil
	}
}

func TestDockerTerminateVerifiesImmutableIDBeforeDelete(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d)
	if ctr.uid != dockerFixtureID {
		t.Fatalf("uid = %q, want the ID docker run printed", ctr.uid)
	}
	inspects := len(d.calls)
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	// The final delete is preceded by a fresh immutable-ID and generation check.
	extra := d.calls[inspects:]
	if len(extra) != 2 || extra[0][0] != "inspect" || extra[1][0] != "rm" {
		t.Errorf("Terminate issued %v, want one verify followed by rm", extra)
	}
	if extra[0][len(extra[0])-1] != dockerFixtureID {
		t.Errorf("verify target = %v, want immutable ID %s", extra[0], dockerFixtureID)
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
	d.bindRunIdentity = true
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
