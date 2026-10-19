package container

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestClientExplicitTargets(t *testing.T) {
	clientA, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient A: %v", err)
	}

	clientB, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-b:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}

	if got := clientA.DefaultHost(); got != "daemon-a" {
		t.Errorf("clientA.DefaultHost() = %q, want daemon-a", got)
	}
	if got := clientB.DefaultHost(); got != "daemon-b" {
		t.Errorf("clientB.DefaultHost() = %q, want daemon-b", got)
	}
	if !clientA.IsRemote() {
		t.Errorf("clientA.IsRemote() = false, want true")
	}
	if !clientB.IsRemote() {
		t.Errorf("clientB.IsRemote() = false, want true")
	}

	if clientA.eng.targetID() == clientB.eng.targetID() {
		t.Errorf("expected different target IDs, got %q", clientA.eng.targetID())
	}
}

func TestTargetPullFlightIsolation(t *testing.T) {
	clientA, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient A: %v", err)
	}

	clientB, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-b:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}

	keyA := flightKey(clientA.eng, "redis:7-alpine", flightPull, "")
	keyB := flightKey(clientB.eng, "redis:7-alpine", flightPull, "")

	if keyA == keyB {
		t.Fatalf("pull flight keys unexpectedly identical across targets: %q", keyA)
	}
}

func TestTargetReuseFlightIsolation(t *testing.T) {
	clientA, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient A: %v", err)
	}

	clientB, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-b:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}

	keyA := clientA.eng.name() + "\x00" + clientA.eng.targetID() + "\x00myctr"
	keyB := clientB.eng.name() + "\x00" + clientB.eng.targetID() + "\x00myctr"

	if keyA == keyB {
		t.Fatalf("reuse flight keys unexpectedly identical across targets: %q", keyA)
	}
}

func TestFlightKeysSeparateBackendsWithoutExplicitTarget(t *testing.T) {
	appleKey := flightKey(appleEngine{}, "redis:7-alpine", flightPull, "")
	dockerKey := flightKey(dockerEngine{}, "redis:7-alpine", flightPull, "")
	if appleKey == dockerKey {
		t.Fatalf("pull flight keys collide across backends: %q", appleKey)
	}
}

func TestCloneRunnerIsolatesBinaryOverride(t *testing.T) {
	orig := &cli.ExecRunner{Binary: "docker", Env: []string{"DOCKER_HOST=tcp://daemon-a:2375"}}
	cloned, ok := cloneRunner(orig).(*cli.ExecRunner)
	if !ok {
		t.Fatal("cloneRunner did not return an ExecRunner")
	}
	applyBinaryToRunner(cloned, "/usr/local/bin/other-docker")

	if orig.Binary != "docker" {
		t.Fatalf("original runner binary mutated to %q", orig.Binary)
	}
	if len(cloned.Env) != 1 || cloned.Env[0] != orig.Env[0] {
		t.Fatalf("cloned env = %q, want %q", cloned.Env, orig.Env)
	}
	if &cloned.Env[0] == &orig.Env[0] {
		t.Fatal("cloned runner shares the original env backing array")
	}
}

func TestEndpointNotDriftingOnEnvChange(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://daemon-a:2375")

	f := newDockerTestRunner()
	inspectJSON := `[{
		"Id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"Config": {
			"Labels": {
				"dev.containergo.managed": "true",
				"dev.containergo.session": "` + sessionID() + `",
				"dev.containergo.creation": "0123456789abcdef"
			}
		},
		"State": {
			"Status": "running",
			"Running": true
		},
		"NetworkSettings": {
			"Networks": {
				"bridge": {
					"IPAddress": "172.17.0.2"
				}
			},
			"Ports": {
				"6379/tcp": [{"HostIp": "0.0.0.0", "HostPort": "16379"}]
			}
		}
	}]`
	f.inspectJSON = inspectJSON

	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientRunner(f),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctr := &Container{
		id:         "test-drift",
		creation:   "0123456789abcdef",
		eng:        client.eng,
		runner:     client.runner,
		targetHost: client.eng.defaultHost(),
		sync:       &lockPack{uid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		exposed:    []portSpec{{proto: "tcp", port: 6379}},
	}

	ctx := context.Background()
	hostBefore, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("Host before env change: %v", err)
	}
	if hostBefore != "daemon-a" {
		t.Fatalf("Host before = %q, want daemon-a", hostBefore)
	}

	endpointBefore, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint before env change: %v", err)
	}
	if !strings.HasPrefix(endpointBefore, "daemon-a:") {
		t.Fatalf("Endpoint before = %q, want host daemon-a", endpointBefore)
	}

	// Change DOCKER_HOST in the process environment.
	t.Setenv("DOCKER_HOST", "tcp://daemon-b:2375")

	hostAfter, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("Host after env change: %v", err)
	}
	if hostAfter != "daemon-a" {
		t.Errorf("Host after env change drifted: got %q, want daemon-a", hostAfter)
	}

	endpointAfter, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint after env change: %v", err)
	}
	if endpointAfter != endpointBefore {
		t.Errorf("Endpoint after env change drifted: got %q, want %q", endpointAfter, endpointBefore)
	}
}

func TestReaperTargetKeyDifferentiation(t *testing.T) {
	cfgA := &config{
		eng:    newDockerEngine("tcp://daemon-a:2375", "", "", ""),
		runner: &cli.ExecRunner{Binary: "docker", Env: []string{"DOCKER_HOST=tcp://daemon-a:2375"}},
	}
	cfgB := &config{
		eng:    newDockerEngine("tcp://daemon-b:2375", "", "", ""),
		runner: &cli.ExecRunner{Binary: "docker", Env: []string{"DOCKER_HOST=tcp://daemon-b:2375"}},
	}

	targetA, okA := runReaperTarget(cfgA)
	targetB, okB := runReaperTarget(cfgB)

	if !okA || !okB {
		t.Fatalf("runReaperTarget returned false: okA=%v, okB=%v", okA, okB)
	}
	if targetA.reaperKey == targetB.reaperKey {
		t.Errorf("reaperKey unexpectedly identical across targets: %q", targetA.reaperKey)
	}
}

func TestWithDockerHostOptionOnRun(t *testing.T) {
	cfg := &config{}
	if err := WithDockerHost("tcp://custom-daemon:2375")(cfg); err != nil {
		t.Fatalf("WithDockerHost: %v", err)
	}
	if cfg.dockerHost == nil || *cfg.dockerHost != "tcp://custom-daemon:2375" {
		t.Errorf("cfg.dockerHost = %v, want tcp://custom-daemon:2375", cfg.dockerHost)
	}

	if err := WithDockerContext("remote-ctx")(cfg); err != nil {
		t.Fatalf("WithDockerContext: %v", err)
	}
	if cfg.dockerContext == nil || *cfg.dockerContext != "remote-ctx" {
		t.Errorf("cfg.dockerContext = %v, want remote-ctx", cfg.dockerContext)
	}

	if err := WithDockerConfig("/custom/cfg")(cfg); err != nil {
		t.Fatalf("WithDockerConfig: %v", err)
	}
	if cfg.dockerConfigDir == nil || *cfg.dockerConfigDir != "/custom/cfg" {
		t.Errorf("cfg.dockerConfigDir = %v, want /custom/cfg", cfg.dockerConfigDir)
	}

	// Validate rejects Docker options when appleEngine is explicitly pinned.
	cfgApple := &config{eng: appleEngine{}, dockerHost: cfg.dockerHost}
	if err := cfgApple.validate(); err == nil {
		t.Errorf("cfgApple.validate() expected error for Docker options on apple engine, got nil")
	}
}

func TestDockerOptionsResolveBackendAndPreserveEnv(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/custom/docker/config")
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "linux/amd64")

	client, err := NewClient(WithClientDockerHost("tcp://daemon-env:2375"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.Backend() != "docker" {
		t.Errorf("client.Backend() = %q, want docker", client.Backend())
	}
	de, ok := client.eng.(dockerEngine)
	if !ok {
		t.Fatalf("client.eng is not dockerEngine: %T", client.eng)
	}
	if de.host != "tcp://daemon-env:2375" {
		t.Errorf("de.host = %q, want tcp://daemon-env:2375", de.host)
	}
	if de.configDir != "/custom/docker/config" {
		t.Errorf("de.configDir = %q, want /custom/docker/config", de.configDir)
	}
	if de.defaultPlatform != "linux/amd64" {
		t.Errorf("de.defaultPlatform = %q, want linux/amd64", de.defaultPlatform)
	}

	// Incompatible backend option returns error.
	_, err = NewClient(WithClientBackend("apple"), WithClientDockerHost("tcp://daemon:2375"))
	if err == nil {
		t.Errorf("NewClient with apple backend and docker host expected error, got nil")
	}
}

func TestWithDockerHostEmptyClearsEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://inherited-daemon:2375")

	cfg := newConfig()
	if err := WithDockerHost("")(cfg); err != nil {
		t.Fatalf("WithDockerHost empty: %v", err)
	}

	// Apply detection logic like in Run
	backend := os.Getenv(backendEnv)
	if backend == "" && cfg.dockerHost != nil {
		backend = "docker"
	}
	eng, err := detectEngineFor(runtime.GOOS, backend)
	if err != nil {
		t.Fatalf("detectEngineFor: %v", err)
	}
	cfg.eng = eng

	if de, ok := cfg.eng.(dockerEngine); ok {
		if !de.explicitTarget {
			if cfg.dockerHost != nil {
				de.host = *cfg.dockerHost
				de.explicitTarget = true
			} else {
				de.host = os.Getenv("DOCKER_HOST")
			}
		}
		if de.host != "" {
			t.Errorf("de.host = %q, want empty string (cleared)", de.host)
		}
		if !de.explicitTarget {
			t.Errorf("de.explicitTarget = false, want true")
		}
	}
}

func TestClientRunRejectsPerRunDockerTargetOverrides(t *testing.T) {
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.Run(context.Background(), "redis:7-alpine", WithDockerHost("tcp://daemon-b:2375"))
	if err == nil {
		t.Errorf("expected client.Run with WithDockerHost override to return error, got nil")
	}
}

func TestClientAppleBackend(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple Container is available only on macOS")
	}
	client, err := NewClient(
		WithClientBackend("apple"),
		WithClientBinary("container"),
	)
	if err != nil {
		t.Fatalf("NewClient apple: %v", err)
	}
	if got := client.Backend(); got != "apple" {
		t.Errorf("client.Backend() = %q, want apple", got)
	}
	if got := client.DefaultHost(); got != "127.0.0.1" {
		t.Errorf("client.DefaultHost() = %q, want 127.0.0.1", got)
	}
	if client.IsRemote() {
		t.Errorf("client.IsRemote() = true, want false")
	}
}

type recordingRunner struct {
	calls [][]string
}

func (r *recordingRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, args)
	if len(args) > 0 && args[0] == "ps" {
		return []byte(""), nil, nil
	}
	return []byte(""), nil, nil
}

func TestClientPullAndPrune(t *testing.T) {
	rec := &recordingRunner{}
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientRunner(rec),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	_ = client.Pull(ctx, "redis:7-alpine")
	if len(rec.calls) == 0 || rec.calls[0][0] != "pull" {
		t.Errorf("expected pull call, got %v", rec.calls)
	}

	pruned, err := client.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("expected empty pruned, got %v", pruned)
	}

	prunedGroup, err := client.PruneReuseGroup(ctx, "test-group")
	if err != nil {
		t.Fatalf("PruneReuseGroup: %v", err)
	}
	if len(prunedGroup) != 0 {
		t.Errorf("expected empty prunedGroup, got %v", prunedGroup)
	}
}
