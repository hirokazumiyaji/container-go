package container

import (
	"bytes"
	"context"
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
	cfg.runner = &dockerRunner{fakeRunner: newTestRunner()}
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

func TestDockerRemoteLoopbackPublishReturnsValidationError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://remote.example:2375")
	cfg := dockerTestConfig(t, WithPublishedPort("127.0.0.1:18080:80"))

	check := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("remote loopback publish was accepted")
		}
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("error = %T %v, want *ValidationError", err, err)
		}
		if !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("error = %v, want ErrInvalidOption", err)
		}
		if validationErr.Option != "WithPublishedPort" {
			t.Errorf("validation option = %q, want WithPublishedPort", validationErr.Option)
		}
	}

	check((dockerEngine{}).checkConfig(context.Background(), cfg))

	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPublishedPort("127.0.0.1:18080:80"),
		withRunner(f), withEngine(dockerEngine{}))
	check(err)
	if len(f.calls) != 0 {
		t.Fatalf("backend was called before remote-publish validation: %v", f.calls)
	}
}

func TestDockerRejectsOneCharacterVolumeNameBeforeBackend(t *testing.T) {
	mount := Mount{Type: MountVolume, Source: "x", Target: "/data"}
	cfg := dockerTestConfig(t, WithMounts(mount))

	check := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("one-character Docker volume name was accepted")
		}
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("error = %T %v, want *ValidationError", err, err)
		}
		if !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("error = %v, want ErrInvalidOption", err)
		}
		if validationErr.Option != "WithMounts" || validationErr.Field != "mount" {
			t.Errorf("validation option/field = %q/%q, want WithMounts/mount", validationErr.Option, validationErr.Field)
		}
	}

	check((dockerEngine{}).checkConfig(context.Background(), cfg))

	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithMounts(mount), withRunner(f), withEngine(dockerEngine{}))
	check(err)
	if len(f.calls) != 0 {
		t.Fatalf("backend was called before Docker volume validation: %v", f.calls)
	}

	// The common grammar remains permissive enough for Apple Container,
	// which accepts one-character volume names.
	if err := (appleEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("Apple backend rejected one-character volume name: %v", err)
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
	if info.networkMode != "bridge" {
		t.Errorf("networkMode = %q, want bridge", info.networkMode)
	}
	if !slices.Equal(info.networkNames, []string{"bridge"}) {
		t.Errorf("networkNames = %v, want [bridge]", info.networkNames)
	}
	if info.image != "redis:7-alpine" {
		t.Errorf("image = %q", info.image)
	}
	want := boundPort{containerPort: 6379, proto: "tcp", hostAddr: "127.0.0.1", hostPort: 49153}
	if !slices.Contains(info.bound, want) {
		t.Errorf("bound = %+v, want to contain %+v", info.bound, want)
	}
}

func TestDockerParseInspectRejectsWrongTarget(t *testing.T) {
	_, err := (dockerEngine{}).parseInspect([]byte(`[{"Id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Name":"/myctr"}]`), dockerFixtureID)
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("parseInspect error = %v, want ErrContainerNotFound", err)
	}
}

func TestDockerParseInspectInfersNetworkMode(t *testing.T) {
	info, err := (dockerEngine{}).parseInspect([]byte(`[
  {"Id":"id","State":{"Status":"running"},"NetworkSettings":{"Networks":{"test-net":{"IPAddress":"172.20.0.2"}}}}
]`), "id")
	if err != nil {
		t.Fatal(err)
	}
	if info.networkMode != "test-net" {
		t.Errorf("networkMode = %q, want test-net", info.networkMode)
	}
}

func TestDockerParseNetworkInspectRequiresMatchingIdentity(t *testing.T) {
	if _, err := parseDockerNetworkInspect([]byte(`[{"Name":"other","Driver":"bridge"}]`), "private"); err == nil {
		t.Fatal("mismatched network name was accepted")
	}
	if _, err := parseDockerNetworkInspect([]byte(`[{"Name":"private"}]`), "private"); err == nil {
		t.Fatal("missing network driver was accepted")
	}
}

func TestDockerStateMapping(t *testing.T) {
	cases := map[string]State{
		"running":    StateRunning,
		"exited":     StateStopped,
		"dead":       StateStopped,
		"created":    StateCreated,
		"restarting": StateRestarting,
		"removing":   StateStopping,
		"paused":     StatePaused,
		"unexpected": StateUnknown,
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
	if got, err := e.stopArgs("myctr", &d); err != nil || !slices.Equal(got, []string{"stop", "--time", "10", "myctr"}) {
		t.Errorf("stopArgs = %v, %v", got, err)
	}
	if got := e.deleteArgs("myctr"); !slices.Equal(got, []string{"rm", "--force", "--volumes", "myctr"}) {
		t.Errorf("deleteArgs = %v", got)
	}
	if e.reaperSubcommand() != "rm" {
		t.Errorf("reaperSubcommand = %q", e.reaperSubcommand())
	}
	if got := e.reaperDeleteFlags(); !slices.Equal(got, []string{"--volumes"}) {
		t.Errorf("reaperDeleteFlags = %v", got)
	}
	if got := e.logsFollowArgs("myctr"); !slices.Equal(got, []string{"logs", "--follow", "myctr"}) {
		t.Errorf("logsFollowArgs = %v", got)
	}
	execArgs := e.execArgs("myctr", &execConfig{user: "u", workdir: "/w"}, "/tmp/env", []string{"id"})
	if !slices.Equal(execArgs, []string{"exec", "--env-file", "/tmp/env", "--user", "u", "--workdir", "/w", "myctr", "id"}) {
		t.Errorf("execArgs = %v", execArgs)
	}
	if got := e.probe().Args; !slices.Equal(got, []string{"version", "--format", "{{.Server.Version}}"}) {
		t.Errorf("probe = %v", got)
	}
}

func TestParseDockerVersionPair(t *testing.T) {
	client, server, err := parseDockerVersionPair([]byte(`{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.2"}}`))
	if err != nil {
		t.Fatalf("parseDockerVersionPair: %v", err)
	}
	if client != (dockerVersion{major: 29, minor: 7, patch: 0}) {
		t.Errorf("client version = %s", client)
	}
	if server != (dockerVersion{major: 29, minor: 7, patch: 2}) {
		t.Errorf("server version = %s", server)
	}
	for _, raw := range []string{
		`{"Client":{"Version":"29.7"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.07.0"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"v29.7.0"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":" 29.7.0 "},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"-29.7.0"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0-rc.1"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0-dev"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0-10-gdeadbee"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0-0.1"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0-"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0-ce"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0+garbage"},"Server":{"Version":"29.7.0"}}`,
		`{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.2+desktop"}}`,
		`{"Client":{"Version":"29.7.0"},"Server":{"Version":"not-a-version"}}`,
	} {
		if _, _, err := parseDockerVersionPair([]byte(raw)); err == nil {
			t.Errorf("parseDockerVersionPair(%s) = nil error", raw)
		}
	}
}

func TestDockerReuseGroupListsFullIDs(t *testing.T) {
	e := dockerEngine{}
	got := e.listReuseGroupArgs("group")
	if !slices.Contains(got, "--no-trunc") || !slices.Contains(got, "{{.ID}}") || slices.Contains(got, "--quiet") {
		t.Errorf("listReuseGroupArgs = %v, want non-truncated {{.ID}} output", got)
	}
	if _, err := e.parseReuseGroupIDs([]byte("group-member\n"), "group"); err == nil {
		t.Error("parseReuseGroupIDs accepted a container name")
	}
}

func TestDockerParseStoppedManaged(t *testing.T) {
	e := dockerEngine{}
	if got := e.listArgs(); !slices.Contains(got, "--filter") {
		t.Errorf("listArgs = %v, want daemon-side filters", got)
	}
	one := strings.Repeat("1", 64)
	two := strings.Repeat("2", 64)
	ids, err := e.parseStoppedManaged([]byte(one + "\n" + two + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{one, two}) {
		t.Errorf("ids = %v", ids)
	}
	if _, err := e.parseStoppedManaged([]byte("short\n")); err == nil {
		t.Fatal("parseStoppedManaged accepted a short ID")
	}
}

// dockerRunner serves docker-shaped responses.
// dockerFixtureID is the Id in testdata/docker_inspect_v29.json, which
// `docker run --detach` also prints on stdout.
const dockerFixtureID = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

type dockerRunner struct {
	*fakeRunner
	serverOS           string
	inspectJSON        []byte
	inspectResponses   [][]byte
	inspectIndex       int
	inspectError       error
	networkInspectJSON []byte
	failInspect        bool
}

func (d *dockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	d.calls = append(d.calls, args)
	switch args[0] {
	case "version":
		for _, a := range args {
			if strings.Contains(a, "json .") {
				return []byte(`{"Client":{"Version":"29.7.0"},"Server":{"Version":"29.7.0"}}`), nil, nil
			}
		}
		if d.serverOS != "" {
			return []byte(d.serverOS), nil, nil
		}
		return []byte("linux"), nil, nil
	case "info":
		return []byte("ok"), nil, nil
	case "network":
		if d.networkInspectJSON != nil {
			return d.networkInspectJSON, nil, nil
		}
		name := args[len(args)-1]
		return []byte(`[{"Name":"` + name + `","Driver":"bridge","Internal":false,"Options":{}}]`), nil, nil
	case "run":
		return []byte(dockerFixtureID + "\n"), nil, nil
	case "inspect":
		if d.failInspect {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected failure"}
		}
		if d.inspectError != nil {
			return nil, nil, d.inspectError
		}
		if len(d.inspectResponses) > 0 {
			index := d.inspectIndex
			if index >= len(d.inspectResponses) {
				index = len(d.inspectResponses) - 1
			}
			d.inspectIndex++
			return d.inspectResponses[index], nil, nil
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
	if d.inspectJSON == nil {
		data, err := os.ReadFile("testdata/docker_inspect_v29.json")
		if err != nil {
			t.Fatal(err)
		}
		d.inspectJSON = data
	}
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

func TestDockerEndpointRefreshesDynamicBinding(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(data, []byte(`"HostPort": "49153"`), []byte(`"HostPort": "49154"`), 1)
	d := &dockerRunner{
		fakeRunner:       newTestRunner(),
		inspectResponses: [][]byte{data, updated},
	}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))

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
	inspectCalls := 0
	versionCalls := 0
	for _, call := range d.calls {
		if len(call) > 0 && call[0] == "version" {
			versionCalls++
		}
		if len(call) > 0 && call[0] == "inspect" {
			inspectCalls++
			if call[len(call)-1] != dockerFixtureID {
				t.Errorf("inspect target = %q, want immutable Docker ID", call[len(call)-1])
			}
		}
	}
	if versionCalls != 1 {
		t.Fatalf("server platform calls = %d, want one cached default lookup", versionCalls)
	}
	if inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want 2", inspectCalls)
	}
	ctr.mu.Lock()
	cached := ctr.info
	ctr.mu.Unlock()
	if cached == nil || cached.ip != "" || cached.networkMode != "" || len(cached.bound) != 0 {
		t.Fatalf("identity cache contains dynamic data: %+v", cached)
	}
}

func TestDockerContainerIPRefreshesDynamicNetwork(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(data, []byte(`"IPAddress": "172.17.0.2"`), []byte(`"IPAddress": "172.17.0.3"`), 1)
	d := &dockerRunner{
		fakeRunner:       newTestRunner(),
		inspectResponses: [][]byte{data, updated},
	}
	ctr := runDockerTestContainer(t, d)
	first, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("first ContainerIP: %v", err)
	}
	second, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("second ContainerIP: %v", err)
	}
	if first != "172.17.0.2" || second != "172.17.0.3" {
		t.Fatalf("ContainerIP values = %q then %q", first, second)
	}
}

func TestDockerEndpointAfterTerminationDoesNotUseCache(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	d.inspectError = &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"inspect", dockerFixtureID},
		ExitCode: 1,
		Stderr:   "Error response from daemon: No such container: " + dockerFixtureID,
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Endpoint after Terminate = %v, want ErrContainerNotFound", err)
	}
}

func TestDockerEndpointUsesUIDWhenNameIsReplaced(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	replacement := bytes.Replace(data, []byte(`"Id": "`+dockerFixtureID+`"`), []byte(`"Id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`), 1)
	d := &dockerRunner{
		fakeRunner:       newTestRunner(),
		inspectResponses: [][]byte{data, replacement},
	}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("first Endpoint: %v", err)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("replacement Endpoint = %v, want ErrContainerNotFound", err)
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
	t.Setenv("DOCKER_HOST", "tcp://[2001:0db8:0000:0000:0000:0000:0000:0005]:2375")
	if got := (dockerEngine{}).defaultHost(); got != "2001:db8::5" {
		t.Errorf("expanded IPv6 defaultHost = %q, want 2001:db8::5", got)
	}
	t.Setenv("DOCKER_HOST", "tcp://[::]:2375")
	if got := (dockerEngine{}).defaultHost(); got != "::1" {
		t.Errorf("IPv6 unspecified defaultHost = %q, want ::1", got)
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

func TestDockerEndpointsRejectLoopbackOnRemoteDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithNetwork("bridge"), WithExposedPorts("6379/tcp"))
	// testdata binds 127.0.0.1:49153 on the daemon. Rewriting that to
	// the remote host would not reach the actual listener.
	ep, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if !errors.Is(err, ErrEndpointUnreachable) {
		t.Fatalf("Endpoint = %q, err = %v; want ErrEndpointUnreachable", ep, err)
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
	for _, addr := range []string{"", "0.0.0.0"} {
		if got := dockerConnectHost(addr, eng); got != "127.0.0.1" {
			t.Errorf("local IPv4 unspecified dockerConnectHost(%q) = %q, want 127.0.0.1", addr, got)
		}
	}
	for _, addr := range []string{"::", "0:0:0:0:0:0:0:0"} {
		if got := dockerConnectHost(addr, eng); got != "::1" {
			t.Errorf("local IPv6 unspecified dockerConnectHost(%q) = %q, want ::1", addr, got)
		}
	}
	for _, tc := range [][2]string{
		{"127.0.0.1", "127.0.0.1"},
		{"127.0.0.2", "127.0.0.2"},
		{"::1", "::1"},
		{"0:0:0:0:0:0:0:1", "::1"},
		{"localhost", "localhost"},
	} {
		if got := dockerConnectHost(tc[0], eng); got != tc[1] {
			t.Errorf("local explicit dockerConnectHost(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

func TestIsRemoteDockerHostUsesFullLoopbackRange(t *testing.T) {
	for _, host := range []string{
		"",
		"unix:///var/run/docker.sock",
		"tcp://127.0.0.1:2375",
		"tcp://127.0.0.2:2375",
		"tcp://[::1]:2375",
		// Docker substitutes defaultAddr.Hostname() for an empty TCP host.
		"tcp://:2375",
		":2375",
	} {
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

// ssh:// is a first-class Docker remote transport, and the daemon there
// resolves bind-mount sources on its own host. Detecting only tcp:// let that
// configuration through unchanged.
func TestIsRemoteDockerHostCoversNonTCPRemoteSchemes(t *testing.T) {
	for _, host := range []string{
		"ssh://user@remote-host",
		"ssh://user@10.0.0.5",
		"http://10.0.0.5:2375",
		"https://10.0.0.5:2376",
	} {
		t.Setenv("DOCKER_HOST", host)
		if !isRemoteDockerHost() {
			t.Errorf("DOCKER_HOST=%q: want remote", host)
		}
	}
	// A Windows named pipe is a local transport.
	t.Setenv("DOCKER_HOST", "npipe:////./pipe/docker_engine")
	if isRemoteDockerHost() {
		t.Error("npipe:// must be local")
	}
	// An ssh host on loopback is still this machine.
	t.Setenv("DOCKER_HOST", "ssh://user@127.0.0.1")
	if isRemoteDockerHost() {
		t.Error("ssh:// to loopback must be local")
	}
	// A malformed value must fail closed rather than permit a bind the
	// daemon would resolve in the wrong place.
	t.Setenv("DOCKER_HOST", "://///")
	if !isRemoteDockerHost() {
		t.Error("a malformed DOCKER_HOST must be treated as remote")
	}
}

// A bind mount must be rejected on an ssh:// daemon, which is the exact
// failure the guard exists to prevent.
func TestDockerRejectsBindMountOnSSHDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://user@remote-host")
	err := (dockerEngine{}).checkConfig(context.Background(), &config{
		mounts: []Mount{{Type: MountBind, Source: "/host/data"}},
	})
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("err = %v, want ErrUnsupportedCapability", err)
	}
}

// Remote-daemon rejections fail closed with distinct sentinels: bind mounts
// carry ErrUnsupportedCapability, loopback publishes carry
// ErrEndpointUnreachable.
func TestDockerRemoteRejectionsShareSentinel(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	eng := dockerEngine{}
	ctx := context.Background()

	bindErr := eng.checkConfig(ctx, &config{mounts: []Mount{{Type: MountBind, Source: "/host/data"}}})
	if !errors.Is(bindErr, ErrUnsupportedCapability) {
		t.Errorf("bind mount err = %v, want ErrUnsupportedCapability", bindErr)
	}

	publishErr := eng.checkConfig(ctx, &config{published: []publishSpec{{hostAddr: "127.0.0.1", raw: "127.0.0.1:18080:80"}}})
	if !errors.Is(publishErr, ErrEndpointUnreachable) {
		t.Errorf("loopback publish err = %v, want ErrEndpointUnreachable", publishErr)
	}
}

func TestNormalizeDockerHostEmptyPort(t *testing.T) {
	if got := normalizeDockerHost(":2375"); got != "tcp://:2375" {
		t.Errorf("normalizeDockerHost(:2375) = %q, want tcp://:2375", got)
	}
	if got := normalizeDockerHost("tcp://:2375"); got != "tcp://:2375" {
		t.Errorf("normalizeDockerHost(tcp://:2375) = %q, want unchanged", got)
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
		if !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, ErrEndpointUnreachable) {
			t.Errorf("WithPublishedPort(%q) error = %v, want config and unreachable sentinels", spec, err)
		}
		var configErr *ConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("WithPublishedPort(%q) error = %v, want *ConfigError", spec, err)
		}
	}
	if len(d.callWith("run")) != 0 {
		t.Errorf("run must not be issued: %v", d.callWith("run"))
	}
	// Unspecified and non-loopback binds stay allowed.
	for _, spec := range []string{"0.0.0.0:18080:80", "10.0.0.5:18080:80", "18080:80"} {
		cfg := dockerTestConfig(t, WithPublishedPort(spec))
		if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
			t.Errorf("checkConfig(%q) = %v, want nil", spec, err)
		}
	}

	t.Setenv("DOCKER_HOST", "")
	cfg := dockerTestConfig(t, WithPublishedPort("127.0.0.1:18080:80"))
	if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
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

func TestDockerRejectsHostNetworkPublishingBeforeRun(t *testing.T) {
	cases := []struct {
		name    string
		network string
		option  Option
		wantErr string
	}{
		{
			name:    "host auto-publish",
			network: "host",
			option:  WithExposedPorts("80/tcp"),
			wantErr: "WithExposedPorts",
		},
		{
			name:    "host explicit publish",
			network: "host",
			option:  WithPublishedPort("127.0.0.1:18080:80"),
			wantErr: "WithPublishedPort",
		},
		{
			name:    "none auto-publish",
			network: "none",
			option:  WithExposedPorts("80/tcp"),
			wantErr: "WithExposedPorts",
		},
		{
			name:    "none explicit publish",
			network: "none",
			option:  WithPublishedPort("127.0.0.1:18080:80"),
			wantErr: "WithPublishedPort",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &dockerRunner{fakeRunner: newTestRunner()}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithNetwork(tc.network), tc.option,
				withRunner(d), withEngine(dockerEngine{}))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want %q", err, tc.wantErr)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Run error = %v, want ErrInvalidConfig", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Errorf("Run error = %v, want *ConfigError", err)
			}
			if len(d.calls) != 0 {
				t.Errorf("CLI called before configuration rejection: %v", d.calls)
			}
		})
	}
}

func TestDockerRunArgsNetworkModeMatrix(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	cases := []struct {
		name        string
		network     string
		opts        []Option
		wantNetwork string
		wantPublish []string
	}{
		{
			name:        "daemon default auto-publish",
			opts:        []Option{WithExposedPorts("80/tcp")},
			wantNetwork: "",
			wantPublish: []string{"127.0.0.1::80/tcp"},
		},
		{
			name:        "bridge auto-publish",
			network:     "bridge",
			opts:        []Option{WithExposedPorts("80/tcp")},
			wantNetwork: "bridge",
			wantPublish: []string{"127.0.0.1::80/tcp"},
		},
		{
			name:        "named auto-publish",
			network:     "test-net",
			opts:        []Option{WithExposedPorts("80/tcp")},
			wantNetwork: "test-net",
			wantPublish: []string{"127.0.0.1::80/tcp"},
		},
		{
			name:        "named explicit publish",
			network:     "test-net",
			opts:        []Option{WithPublishedPort("127.0.0.1:18080:80")},
			wantNetwork: "test-net",
			wantPublish: []string{"127.0.0.1:18080:80"},
		},
		{
			name:        "host no publish",
			network:     "host",
			wantNetwork: "host",
		},
		{
			name:        "none no publish",
			network:     "none",
			wantNetwork: "none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option(nil), tc.opts...)
			if tc.network != "" {
				opts = append(opts, WithNetwork(tc.network))
			}
			cfg := dockerTestConfig(t, opts...)
			eng := dockerEngine{}
			if err := eng.checkConfig(context.Background(), cfg); err != nil {
				t.Fatalf("checkConfig: %v", err)
			}
			args := eng.runArgs(cfg, "redis:7-alpine", "")
			if got := argvValue(args, "--network"); got != tc.wantNetwork {
				t.Errorf("--network = %q, want %q (args=%v)", got, tc.wantNetwork, args)
			}
			if got := argvValues(args, "--publish"); !slices.Equal(got, tc.wantPublish) {
				t.Errorf("publish args = %v, want %v (args=%v)", got, tc.wantPublish, args)
			}
		})
	}
}

func TestDockerRunArgsNeverPublishInNonPublishableNetwork(t *testing.T) {
	for _, network := range []string{"host", "none"} {
		t.Run(network, func(t *testing.T) {
			cfg := dockerTestConfig(t,
				WithNetwork(network),
				WithExposedPorts("80/tcp"),
				WithPublishedPort("127.0.0.1:18080:80"),
			)
			if got := argvValues(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), "--publish"); len(got) != 0 {
				t.Fatalf("publish args = %v, want none", got)
			}
		})
	}
}

func TestDockerReuseRejectsNetworkModeMismatch(t *testing.T) {
	cfg := dockerTestConfig(t, WithName("myctr"), WithReuse(), WithNetwork("bridge"))
	info := &engineInfo{
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "0123456789abcdef",
		},
		uid:          dockerFixtureID,
		image:        "redis:7-alpine",
		networkMode:  dockerNetworkHost,
		networkNames: []string{dockerNetworkBridge},
	}
	if err := checkReuseCompat(info, "redis:7-alpine", cfg); err == nil || !strings.Contains(err.Error(), "network mode") {
		t.Fatalf("checkReuseCompat error = %v, want network mode mismatch", err)
	}
}

func TestDockerExplicitEndpointUsesInspectBinding(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	d.inspectJSON = data
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithNetwork("bridge"),
		WithPublishedPort("127.0.0.1:49153:6379"),
		withRunner(d), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	host, err := ctr.Host(context.Background())
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("Host = %q, err = %v", host, err)
	}
	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil || endpoint != "127.0.0.1:49153" {
		t.Fatalf("Endpoint = %q, err = %v", endpoint, err)
	}
}

func TestDockerEndpointDoesNotTrustRequestedBinding(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	d.inspectJSON = data
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithNetwork("bridge"),
		WithPublishedPort("127.0.0.1:18080:80"),
		withRunner(d), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if host, err := ctr.Host(context.Background()); err == nil {
		t.Errorf("Host = %q, want an error for a binding absent from inspect", host)
	}
	if endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp"); err == nil {
		t.Errorf("Endpoint = %q, want an error for a binding absent from inspect", endpoint)
	}
}

func TestDockerEndpointNetworkModeMatrix(t *testing.T) {
	cases := []struct {
		mode         string
		wantEndpoint string
		wantErr      bool
	}{
		{mode: "bridge", wantEndpoint: "127.0.0.1:49153"},
		{mode: "host", wantErr: true},
		{mode: "none", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			d := &dockerRunner{fakeRunner: newTestRunner()}
			inspectJSON := ""
			if tc.mode == dockerNetworkNone {
				// Docker creates no endpoint for none networking, so the
				// reported network map is empty. A synthetic "none"
				// entry would hide mode-check regressions.
				inspectJSON = string(dockerNoneNetworkInspect(t))
			} else {
				data, err := os.ReadFile("testdata/docker_inspect_v29.json")
				if err != nil {
					t.Fatal(err)
				}
				inspectJSON = strings.Replace(string(data), `"NetworkMode": "bridge"`, `"NetworkMode": "`+tc.mode+`"`, 1)
				inspectJSON = strings.Replace(inspectJSON, `"bridge": {`, `"`+tc.mode+`": {`, 1)
			}
			d.inspectJSON = []byte(inspectJSON)
			opts := []Option{WithName("myctr"), WithNetwork(tc.mode), withRunner(d), withEngine(dockerEngine{})}
			if tc.mode == "bridge" {
				opts = append(opts, WithExposedPorts("6379/tcp"))
			}
			ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.mode != "bridge" {
				// The public API rejects declarations on host/none. Add
				// one only to exercise the defensive endpoint resolver.
				ctr.exposed = []portSpec{{port: 6379, proto: "tcp"}}
			}
			host, hostErr := ctr.Host(context.Background())
			endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
			if tc.wantErr {
				if tc.mode == "none" {
					if !errors.Is(hostErr, ErrNoReachableHost) {
						t.Errorf("none Host error = %v, want ErrNoReachableHost", hostErr)
					}
				} else if hostErr != nil || host != "127.0.0.1" {
					t.Errorf("host-mode Host = %q, err = %v; want 127.0.0.1", host, hostErr)
				}
				if err == nil {
					t.Fatalf("Endpoint = %q, want %s network mode error", endpoint, tc.mode)
				}
				if tc.mode == "none" && (!errors.Is(err, ErrPortNotExposed) || !errors.Is(err, ErrNoReachableHost)) {
					t.Errorf("none Endpoint error = %v, want port and host sentinels", err)
				}
				return
			}
			if hostErr != nil || host != "127.0.0.1" {
				t.Errorf("Host = %q, err = %v; want 127.0.0.1", host, hostErr)
			}
			if err != nil || endpoint != tc.wantEndpoint {
				t.Errorf("Endpoint = %q, err = %v; want %q", endpoint, err, tc.wantEndpoint)
			}
		})
	}
}

// dockerNoneNetworkInspect mirrors what a real Docker daemon reports for
// a --network none container: no endpoint exists, so the reported network
// map and port table are empty and only HostConfig.NetworkMode names the
// mode.
func dockerNoneNetworkInspect(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	inspect := strings.Replace(string(data), `"NetworkMode": "bridge"`, `"NetworkMode": "none"`, 1)
	inspect = strings.Replace(inspect, `"IPAddress": "172.17.0.2"`, `"IPAddress": ""`, 1)
	inspect = strings.Replace(inspect, `"Networks": {
        "bridge": {
          "IPAddress": "172.17.0.2",
          "Gateway": "172.17.0.1"
        }
      }`, `"Networks": {}`, 1)
	inspect = strings.Replace(inspect, `"Ports": {
        "6379/tcp": [
          { "HostIp": "127.0.0.1", "HostPort": "49153" }
        ],
        "8080/tcp": null
      }`, `"Ports": {}`, 1)
	if strings.Contains(inspect, `"bridge"`) || strings.Contains(inspect, "49153") {
		t.Fatal("none-network fixture still carries a bridge endpoint or binding")
	}
	return []byte(inspect)
}

func TestDockerNoneNetworkReportsModeSpecificErrors(t *testing.T) {
	d := &dockerRunner{
		fakeRunner:       newTestRunner(),
		serverOS:         "linux",
		inspectResponses: [][]byte{dockerNoneNetworkInspect(t)},
	}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithNetwork(dockerNetworkNone), withRunner(d), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := ctr.Host(context.Background()); !errors.Is(err, ErrNoReachableHost) || errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("none-mode Host error = %v, want ErrNoReachableHost without ErrNetworkMismatch", err)
	}
	// The public API rejects declarations in none mode; add one only to
	// exercise the defensive endpoint resolver.
	ctr.exposed = []portSpec{{port: 6379, proto: "tcp"}}
	_, err = ctr.Endpoint(context.Background(), "6379/tcp")
	if !errors.Is(err, ErrNoReachableHost) || !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("none-mode Endpoint error = %v, want port and host sentinels", err)
	}
}

func TestDockerRejectsIsolatedNetworkPublishingBeforeRun(t *testing.T) {
	cases := []struct {
		name         string
		networkJSON  string
		wantContains string
	}{
		{
			name:         "internal",
			networkJSON:  `[{"Name":"private","Driver":"bridge","Internal":true,"Options":{}}]`,
			wantContains: "internal",
		},
		{
			name:         "isolated gateway",
			networkJSON:  `[{"Name":"private","Driver":"bridge","Internal":false,"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated"}}]`,
			wantContains: "isolated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, option := range []Option{WithExposedPorts("80/tcp"), WithPublishedPort("127.0.0.1:18080:80")} {
				d := &dockerRunner{
					fakeRunner:         newTestRunner(),
					networkInspectJSON: []byte(tc.networkJSON),
				}
				_, err := Run(context.Background(), "redis:7-alpine",
					WithName("myctr"), WithNetwork("private"), option,
					withRunner(d), withEngine(dockerEngine{}))
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantContains) {
					t.Fatalf("Run error = %v, want %s rejection", err, tc.wantContains)
				}
				if !errors.Is(err, ErrInvalidConfig) {
					t.Errorf("Run error = %v, want ErrInvalidConfig", err)
				}
				var configErr *ConfigError
				if !errors.As(err, &configErr) {
					t.Errorf("Run error = %v, want *ConfigError", err)
				} else if configErr.Backend != "docker" || configErr.Network != "private" || configErr.Option == "" {
					t.Errorf("ConfigError = %+v, want docker/private/option fields", configErr)
				}
				if len(d.callWith("run")) != 0 || len(d.callWith("image")) != 0 {
					t.Fatalf("CLI started image/container before config rejection: %v", d.calls)
				}
				if networkCall := d.callWith("network"); len(networkCall) < 3 || networkCall[1] != "inspect" || networkCall[2] != "private" {
					t.Fatalf("network inspect calls = %v, want inspect private", d.calls)
				}
			}
		})
	}
}

func TestDockerIPv6BindingsAreCanonicalAndPreserveFamily(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		publishSpec  string
		hostIP       string
		wantHost     string
		wantEndpoint string
	}{
		{
			name:         "expanded loopback",
			publishSpec:  "[0:0:0:0:0:0:0:1]:49153:6379/tcp",
			hostIP:       "::1",
			wantHost:     "::1",
			wantEndpoint: "[::1]:49153",
		},
		{
			name:         "IPv6 unspecified",
			publishSpec:  "[::]:49153:6379/tcp",
			hostIP:       "::",
			wantHost:     "::1",
			wantEndpoint: "[::1]:49153",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inspectJSON := strings.Replace(string(data), `"HostIp": "127.0.0.1"`, `"HostIp": "`+tc.hostIP+`"`, 1)
			d := &dockerRunner{fakeRunner: newTestRunner()}
			ctr := runDockerTestContainer(t, d,
				WithNetwork("bridge"), WithPublishedPort(tc.publishSpec))
			d.inspectJSON = []byte(inspectJSON)
			ctr.info = nil
			host, err := ctr.Host(context.Background())
			if err != nil || host != tc.wantHost {
				t.Fatalf("Host = %q, err = %v; want %q", host, err, tc.wantHost)
			}
			endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
			if err != nil || endpoint != tc.wantEndpoint {
				t.Fatalf("Endpoint = %q, err = %v; want %q", endpoint, err, tc.wantEndpoint)
			}
		})
	}
}

func TestDockerOmittedNetworkPreservesDaemonDefault(t *testing.T) {
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))
	if ctr.network != "" || ctr.networkExplicit {
		t.Errorf("Container network = %q explicit=%t, want daemon default", ctr.network, ctr.networkExplicit)
	}
	if got := argvValue(d.callWith("run"), "--network"); got != "" {
		t.Errorf("--network = %q, want omitted", got)
	}
	if d.callWith("network") != nil {
		t.Errorf("daemon default should not be preflight-inspected as bridge: %v", d.calls)
	}

	// A stale internal normalization must not turn an implicit request
	// into an explicit network mode.
	cfg := dockerTestConfig(t, WithExposedPorts("6379/tcp"))
	cfg.network = dockerNetworkHost
	cfg.networkExplicit = false
	if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("implicit network was interpreted as %q: %v", cfg.network, err)
	}
	if got := argvValue((dockerEngine{}).runArgs(cfg, "redis:7-alpine", ""), "--network"); got != "" {
		t.Fatalf("implicit network synthesized --network %q", got)
	}
}

func TestDockerOmittedDefaultEndpointRejectsHostFallback(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		mode     string
		network  string
		serverOS string
	}{
		{name: "host", mode: dockerNetworkHost, network: dockerNetworkBridge, serverOS: "linux"},
		{name: "none", mode: dockerNetworkNone, network: dockerNetworkBridge, serverOS: "linux"},
		{name: "linux user-defined nat", mode: dockerNetworkNAT, network: dockerNetworkNAT, serverOS: "linux"},
		{name: "windows user-defined bridge", mode: dockerNetworkBridge, network: dockerNetworkBridge, serverOS: "windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspect := []byte(strings.Replace(string(data), `"NetworkMode": "bridge"`, `"NetworkMode": "`+tc.mode+`"`, 1))
			inspect = []byte(strings.Replace(string(inspect), `"bridge": {`, `"`+tc.network+`": {`, 1))
			d := &dockerRunner{
				fakeRunner:       newTestRunner(),
				serverOS:         tc.serverOS,
				inspectResponses: [][]byte{inspect},
			}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithExposedPorts("6379/tcp"),
				withRunner(d), withEngine(dockerEngine{}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); !errors.Is(err, ErrNetworkMismatch) {
				t.Fatalf("Endpoint error = %v, want ErrNetworkMismatch", err)
			}
		})
	}
}

func TestDockerDefaultNATEndpointUsesActualNetworkName(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"NetworkMode": "bridge"`), []byte(`"NetworkMode": "default"`), 1)
	data = bytes.Replace(data, []byte(`"bridge": {`), []byte(`"nat": {`), 1)
	d := &dockerRunner{
		fakeRunner:       newTestRunner(),
		serverOS:         "windows",
		inspectResponses: [][]byte{data},
	}
	ctr := runDockerTestContainer(t, d, WithExposedPorts("6379/tcp"))
	endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil || endpoint != "127.0.0.1:49153" {
		t.Fatalf("NAT default endpoint = %q, err = %v", endpoint, err)
	}
}

func TestDockerExplicitNATNetworkIsPassedThrough(t *testing.T) {
	cfg := dockerTestConfig(t, WithNetwork(dockerNetworkNAT), WithExposedPorts("6379/tcp"))
	if got := argvValue(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), "--network"); got != dockerNetworkNAT {
		t.Fatalf("--network = %q, want %q", got, dockerNetworkNAT)
	}
}

func TestDockerExplicitDefaultUsesDockerSpecialMode(t *testing.T) {
	cfg := dockerTestConfig(t, WithNetwork(dockerNetworkDefault), WithExposedPorts("6379/tcp"))
	if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("checkConfig(default): %v", err)
	}
	if got := argvValue(dockerEngine{}.runArgs(cfg, "redis:7-alpine", ""), "--network"); got != dockerNetworkDefault {
		t.Fatalf("--network = %q, want %q", got, dockerNetworkDefault)
	}
}

func TestDockerReuseRejectsOmittedNetworkWildcard(t *testing.T) {
	for _, actual := range []string{"host", "none", "test-net", "default", ""} {
		t.Run("actual="+actual, func(t *testing.T) {
			cfg := dockerTestConfig(t, WithName("myctr"), WithReuse())
			names := []string(nil)
			if actual != "" {
				names = []string{actual}
			}
			if actual == dockerNetworkDefault {
				names = []string{dockerNetworkBridge}
			}
			info := &engineInfo{
				labels: map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: "0123456789abcdef",
				},
				uid:          dockerFixtureID,
				image:        "redis:7-alpine",
				networkMode:  actual,
				networkNames: names,
			}
			if err := checkReuseCompat(info, "redis:7-alpine", cfg); err == nil || !errors.Is(err, ErrNetworkMismatch) {
				t.Fatalf("checkReuseCompat error = %v, want ErrNetworkMismatch", err)
			}
		})
	}
}

func TestDockerReuseCanonicalizesDaemonDefaultNetwork(t *testing.T) {
	for _, tc := range []struct {
		name           string
		actual         string
		names          []string
		defaultNetwork string
	}{
		{name: "linux bridge", actual: dockerNetworkDefault, names: []string{dockerNetworkBridge}, defaultNetwork: dockerNetworkBridge},
		{name: "windows nat", actual: dockerNetworkDefault, names: []string{dockerNetworkNAT}, defaultNetwork: dockerNetworkNAT},
		{name: "reported bridge", actual: dockerNetworkBridge, names: []string{dockerNetworkBridge}, defaultNetwork: dockerNetworkBridge},
		{name: "reported nat", actual: dockerNetworkNAT, names: []string{dockerNetworkNAT}, defaultNetwork: dockerNetworkNAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dockerTestConfig(t, WithName("myctr"), WithReuse())
			info := &engineInfo{
				labels: map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: "0123456789abcdef",
				},
				uid:            dockerFixtureID,
				image:          "redis:7-alpine",
				networkMode:    tc.actual,
				networkNames:   tc.names,
				defaultNetwork: tc.defaultNetwork,
			}
			if err := checkReuseCompat(info, "redis:7-alpine", cfg); err != nil {
				t.Fatalf("checkReuseCompat: %v", err)
			}
		})
	}
}

func TestDockerOmittedNetworkRejectsUserDefinedPlatformName(t *testing.T) {
	for _, tc := range []struct {
		name           string
		actual         string
		names          []string
		defaultNetwork string
	}{
		{name: "linux user-defined nat", actual: dockerNetworkNAT, names: []string{dockerNetworkNAT}, defaultNetwork: dockerNetworkBridge},
		{name: "windows user-defined bridge", actual: dockerNetworkBridge, names: []string{dockerNetworkBridge}, defaultNetwork: dockerNetworkNAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dockerTestConfig(t, WithName("myctr"), WithReuse())
			info := &engineInfo{
				labels: map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: "0123456789abcdef",
				},
				uid:            dockerFixtureID,
				image:          "redis:7-alpine",
				networkMode:    tc.actual,
				networkNames:   tc.names,
				defaultNetwork: tc.defaultNetwork,
			}
			if err := checkReuseCompat(info, "redis:7-alpine", cfg); !errors.Is(err, ErrNetworkMismatch) {
				t.Fatalf("checkReuseCompat error = %v, want ErrNetworkMismatch", err)
			}
		})
	}
}

func TestDockerReuseRejectsRemoteLoopbackBinding(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	cfg := dockerTestConfig(t,
		WithName("myctr"), WithReuse(), WithNetwork("bridge"),
		WithExposedPorts("6379/tcp"),
	)
	info := &engineInfo{
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "0123456789abcdef",
		},
		uid:          dockerFixtureID,
		image:        "redis:7-alpine",
		networkMode:  "bridge",
		networkNames: []string{"bridge"},
		bound: []boundPort{{
			containerPort: 6379,
			proto:         "tcp",
			hostAddr:      "0:0:0:0:0:0:0:1",
			hostPort:      49153,
		}},
	}
	err := checkReuseCompat(info, "redis:7-alpine", cfg)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("checkReuseCompat error = %v, want unreachable loopback rejection", err)
	}
	if !errors.Is(err, ErrEndpointUnreachable) {
		t.Errorf("checkReuseCompat error = %v, want ErrEndpointUnreachable", err)
	}
}

func TestDockerHostModeHostReturnsDaemonHostWithoutInventingPort(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	inspectJSON := strings.Replace(string(data), `"NetworkMode": "bridge"`, `"NetworkMode": "host"`, 1)
	inspectJSON = strings.Replace(inspectJSON, `"bridge": {`, `"host": {`, 1)
	d := &dockerRunner{fakeRunner: newTestRunner()}
	ctr := runDockerTestContainer(t, d, WithNetwork("host"))
	d.inspectJSON = []byte(inspectJSON)
	ctr.info = nil
	host, err := ctr.Host(context.Background())
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("Host = %q, err = %v; want daemon host", host, err)
	}
	if endpoint, err := ctr.Endpoint(context.Background(), "6379/tcp"); err == nil {
		t.Fatalf("Endpoint = %q, want host-network port inference to fail", endpoint)
	}
}

func argvValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func argvValues(args []string, flag string) []string {
	var values []string
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			values = append(values, args[i+1])
		}
	}
	return values
}
