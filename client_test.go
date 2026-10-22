package container

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestRunRejectsDockerTargetOptionsOnDetectedAppleBackend(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple Container is available only on macOS")
	}
	t.Setenv(backendEnv, "apple")
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithDockerHost("tcp://daemon-a:2375"), withRunner(f))
	if err == nil {
		t.Fatal("Run with WithDockerHost on the Apple backend returned no error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("Run invoked the backend before rejecting the option: %v", f.calls)
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

func TestClientPinsAbsentDockerEnvComponents(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")

	client, err := NewClient(WithClientBackend("docker"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	er, ok := client.runner.(*cli.ExecRunner)
	if !ok {
		t.Fatalf("client.runner is %T, want *cli.ExecRunner", client.runner)
	}

	// The client froze the default local target, so a DOCKER_HOST exported
	// after construction must not reach the child CLI.
	t.Setenv("DOCKER_HOST", "tcp://late-daemon:2375")
	for _, entry := range cli.MergeEnv(os.Environ(), er.Env) {
		if strings.HasPrefix(entry, "DOCKER_HOST=") {
			t.Fatalf("client runner inherited a later DOCKER_HOST: %q", entry)
		}
	}
}

func TestRunPinsAbsentDockerEnvComponents(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")

	cfg := &config{eng: dockerEngine{}, runner: &cli.ExecRunner{Binary: "docker"}}
	applyEngineRunnerEnv(cfg)
	er, ok := cfg.runner.(*cli.ExecRunner)
	if !ok {
		t.Fatalf("cfg.runner is %T, want *cli.ExecRunner", cfg.runner)
	}

	t.Setenv("DOCKER_HOST", "tcp://late-daemon:2375")
	for _, entry := range cli.MergeEnv(os.Environ(), er.Env) {
		if strings.HasPrefix(entry, "DOCKER_HOST=") {
			t.Fatalf("run runner inherited a later DOCKER_HOST: %q", entry)
		}
	}
}

func TestClientBinaryIsTrimmed(t *testing.T) {
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientBinary("  /opt/bin/docker  "),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	er, ok := client.runner.(*cli.ExecRunner)
	if !ok {
		t.Fatalf("client.runner is %T, want *cli.ExecRunner", client.runner)
	}
	if er.Binary != "/opt/bin/docker" {
		t.Errorf("client binary = %q, want /opt/bin/docker", er.Binary)
	}
}

func TestDockerTargetValuesRejectUnsafeInput(t *testing.T) {
	cfg := newConfig()
	long := strings.Repeat("a", cli.MaxStreamOverlap+1)

	for _, tt := range []struct {
		name string
		opt  Option
	}{
		{"host line break", WithDockerHost("tcp://daemon\ninjected")},
		{"host NUL", WithDockerHost("tcp://daemon\x00")},
		{"oversized context", WithDockerContext(long)},
		{"oversized config", WithDockerConfig(long)},
	} {
		if err := tt.opt(cfg); err == nil {
			t.Errorf("%s: option accepted an unsafe value", tt.name)
		}
	}
}

func TestDockerTargetValuesAreRedacted(t *testing.T) {
	const host = "tcp://user:secret-token@daemon-a:2375"

	cfg := &config{}
	if err := WithDockerHost(host)(cfg); err != nil {
		t.Fatalf("WithDockerHost: %v", err)
	}
	err := cfg.publicError(errors.New("cannot connect to " + host))
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("docker host credentials leaked into public error: %v", err)
	}
}

func TestClientTargetValuesAreRedacted(t *testing.T) {
	const host = "tcp://user:secret-token@daemon-a:2375"

	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost(host),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Client.Run freezes the target on the engine, so the engine-resolved
	// value must be redacted from public errors too.
	cfg := &config{eng: client.eng}
	err = cfg.publicError(errors.New("cannot connect to " + host))
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("client target credentials leaked into public error: %v", err)
	}
}

func TestOversizedDockerTargetValueIsNotRegisteredForRedaction(t *testing.T) {
	huge := "tcp://" + strings.Repeat("a", cli.MaxStreamOverlap)
	cfg := &config{eng: newDockerEngine(huge, "", "", "")}
	for _, value := range cfg.diagnosticValues() {
		if len(value) > cli.MaxStreamOverlap {
			t.Fatalf("oversized docker target value reached the stream redactor: %d bytes", len(value))
		}
	}

	cfg = &config{eng: newDockerEngine("tcp://daemon-a:2375", "", "", "")}
	found := false
	for _, value := range cfg.diagnosticValues() {
		if value == "tcp://daemon-a:2375" {
			found = true
		}
	}
	if !found {
		t.Error("engine-resolved docker target value is missing from the redaction registry")
	}
}

func TestDockerTargetValidationNamesTheOption(t *testing.T) {
	dockerCtx, dir := "remote-ctx", "/custom/cfg"

	contextOnly := &config{eng: appleEngine{}, dockerContext: &dockerCtx}
	var valErr *ValidationError
	if err := contextOnly.validate(); !errors.As(err, &valErr) || valErr.Option != "WithDockerContext" {
		t.Errorf("validate() = %v, want a ValidationError for WithDockerContext", err)
	}

	nonDocker := &config{eng: appleEngine{}, dockerConfigDir: &dir}
	valErr = nil
	if err := nonDocker.validate(); !errors.As(err, &valErr) || valErr.Option != "WithDockerConfig" {
		t.Errorf("validate() = %v, want a ValidationError for WithDockerConfig", err)
	}
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

func TestNewClientRejectsNilOption(t *testing.T) {
	_, err := NewClient(nil)
	assertPublicValidationError(t, err, "NewClient", "NewClient", 0, "option 0 is nil")

	_, err = NewClientContext(context.Background(), nil)
	assertPublicValidationError(t, err, "NewClientContext", "NewClientContext", 0, "option 0 is nil")
}

func TestClientOptionsReturnValidationErrors(t *testing.T) {
	var typedNil *recordingRunner

	for _, tt := range []struct {
		name    string
		opt     ClientOption
		option  string
		value   any
		message string
	}{
		{
			name:    "empty binary",
			opt:     WithClientBinary("   "),
			option:  "WithClientBinary",
			value:   "",
			message: "binary must not be empty",
		},
		{
			name:    "unsafe binary",
			opt:     WithClientBinary("/opt/bin/docker\ninjected"),
			option:  "WithClientBinary",
			value:   "/opt/bin/docker\ninjected",
			message: `invalid binary path "/opt/bin/docker\ninjected"`,
		},
		{
			name:    "nil runner",
			opt:     WithClientRunner(nil),
			option:  "WithClientRunner",
			value:   nil,
			message: "runner must not be nil",
		},
		{
			name:    "typed nil runner",
			opt:     WithClientRunner(typedNil),
			option:  "WithClientRunner",
			value:   nil,
			message: "runner must not be nil",
		},
		{
			name:    "invalid backend",
			opt:     WithClientBackend("podman"),
			option:  "WithClientBackend",
			value:   nil,
			message: `invalid CONTAINERGO_BACKEND="podman": valid values are "apple" and "docker"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opt(&clientConfig{})
			assertPublicValidationError(t, err, tt.option, tt.option, tt.value, tt.message)
		})
	}
}

func TestNewClientReturnsTypedErrorForUnsafeOption(t *testing.T) {
	_, err := NewClient(WithClientBinary("   "))
	assertPublicValidationError(t, err, "WithClientBinary", "WithClientBinary", "", "binary must not be empty")
}

// contextRecordingRunner reports the context it was given and otherwise
// answers like a Docker CLI that can inspect a context.
type contextRecordingRunner struct {
	calls int
}

func (r *contextRecordingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.calls++
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return []byte(`[{"Name":"ctx","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil, nil
}

func TestNewClientContextBoundsTargetResolution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := &contextRecordingRunner{}
	_, err := NewClientContext(ctx,
		WithClientBackend("docker"),
		WithClientDockerContext("ctx"),
		WithClientRunner(runner),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewClientContext error = %v, want context.Canceled", err)
	}
	if runner.calls != 1 {
		t.Fatalf("context resolution calls = %d, want 1", runner.calls)
	}
}

// deadlinePullRunner records the deadline of the pull invocation. Other
// operations (such as backend target resolution) are answered with empty
// output, which makes Docker fall back to its local target.
type deadlinePullRunner struct {
	pullDeadline time.Duration
}

func (r *deadlinePullRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "pull" {
		if deadline, ok := ctx.Deadline(); ok {
			r.pullDeadline = time.Until(deadline)
		}
	}
	return nil, nil, nil
}

func TestPullOptionsAreHonored(t *testing.T) {
	t.Setenv(backendEnv, "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	t.Run("platform", func(t *testing.T) {
		f := newDockerTestRunner()
		if err := Pull(context.Background(), "redis:7-alpine",
			WithPullRunner(f), WithPullPlatform("linux/arm64")); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if pull := f.callWith("pull"); !hasPlatformArg(pull, "linux/arm64") {
			t.Fatalf("pull did not use the requested platform: %v", f.calls)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		r := &deadlinePullRunner{}
		if err := Pull(context.Background(), "redis:7-alpine",
			WithPullRunner(r), WithPullTimeout(2*time.Second)); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if r.pullDeadline < time.Second || r.pullDeadline > 3*time.Second {
			t.Fatalf("pull deadline = %v, want about 2s", r.pullDeadline)
		}
	})

	t.Run("docker host", func(t *testing.T) {
		f := newDockerTestRunner()
		if err := Pull(context.Background(), "redis:7-alpine",
			WithPullRunner(f), WithPullDockerHost("tcp://daemon-a:2375")); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if f.callWith("pull") == nil {
			t.Fatalf("pull was not called: %v", f.calls)
		}
	})

	t.Run("docker context", func(t *testing.T) {
		f := newDockerTestRunner()
		f.contextJSON = `[{"Name":"ctx","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`
		if err := Pull(context.Background(), "redis:7-alpine",
			WithPullRunner(f), WithPullDockerContext("ctx")); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if call := f.callWith("context"); len(call) != 3 || call[1] != "inspect" || call[2] != "ctx" {
			t.Fatalf("context was not inspected: %v", f.calls)
		}
		if f.callWith("pull") == nil {
			t.Fatalf("pull was not called: %v", f.calls)
		}
	})
}

func TestPullDockerTargetRequiresDockerBackend(t *testing.T) {
	host := "tcp://daemon-a:2375"
	_, err := (pullConfig{dockerHost: &host}).applyDockerTarget(appleEngine{})
	assertPublicValidationError(t, err, "WithPullDockerHost", "WithPullDockerHost", nil,
		"Docker target options require Docker backend")
}

func TestClientPullHonorsOptionsAndRejectsTargetOverride(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	f := newDockerTestRunner()
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientRunner(f),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := client.Pull(context.Background(), "redis:7-alpine", WithPullPlatform("linux/arm64")); err != nil {
		t.Fatalf("Client.Pull: %v", err)
	}
	if pull := f.callWith("pull"); !hasPlatformArg(pull, "linux/arm64") {
		t.Fatalf("Client.Pull did not use the requested platform: %v", f.calls)
	}

	before := len(f.calls)
	err = client.Pull(context.Background(), "redis:7-alpine", WithPullDockerHost("tcp://daemon-b:2375"))
	assertPublicValidationError(t, err, "WithPullDockerHost", "WithPullDockerHost", nil,
		"cannot override Docker target options on Client.Pull")
	if len(f.calls) != before {
		t.Fatalf("Client.Pull invoked the backend after rejecting a target override: %v", f.calls)
	}
}

func TestClientPruneHonorsOptions(t *testing.T) {
	pinned := &recordingRunner{}
	override := &recordingRunner{}
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientRunner(pinned),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	removed, err := client.Prune(context.Background(),
		WithPruneRunner(override), WithPruneTimeout(time.Second), WithPruneParallelism(1))
	if err != nil {
		t.Fatalf("Client.Prune: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty", removed)
	}
	if len(override.calls) == 0 {
		t.Fatal("Client.Prune did not use the option runner")
	}
	if len(pinned.calls) != 0 {
		t.Fatalf("Client.Prune used the pinned runner despite WithPruneRunner: %v", pinned.calls)
	}

	groupRunner := &recordingRunner{}
	removed, err = client.PruneReuseGroup(context.Background(), "test-group",
		WithPruneRunner(groupRunner), WithPruneTimeout(time.Second))
	if err != nil {
		t.Fatalf("Client.PruneReuseGroup: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty", removed)
	}
	if len(groupRunner.calls) == 0 {
		t.Fatal("Client.PruneReuseGroup did not use the option runner")
	}
}

func TestClientPruneRejectsNilOption(t *testing.T) {
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientRunner(&recordingRunner{}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.Prune(context.Background(), nil)
	assertPublicValidationError(t, err, "Client.Prune", "Client.Prune", 0, "option 0 is nil")

	_, err = client.PruneReuseGroup(context.Background(), "test-group", nil)
	assertPublicValidationError(t, err, "Client.PruneReuseGroup", "Client.PruneReuseGroup", 0, "option 0 is nil")
}

func TestPullDockerTargetImpliesDockerBackend(t *testing.T) {
	t.Setenv(backendEnv, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	f := newDockerTestRunner()
	if err := Pull(context.Background(), "redis:7-alpine",
		WithPullRunner(f), WithPullDockerHost("tcp://daemon-a:2375")); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if f.callWith("pull") == nil {
		t.Fatalf("pull was not called: %v", f.calls)
	}
}

func TestNewClientContextReturnsCancellationWithoutExplicitTarget(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := &contextRecordingRunner{}
	_, err := NewClientContext(ctx,
		WithClientBackend("docker"),
		WithClientRunner(runner),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewClientContext error = %v, want context.Canceled", err)
	}
	if runner.calls != 1 {
		t.Fatalf("context resolution calls = %d, want 1", runner.calls)
	}
}

func TestNewClientRedactsEnvironmentDockerTarget(t *testing.T) {
	const host = "tcp://daemon-secret.invalid:bad port"
	t.Setenv("DOCKER_HOST", host)
	t.Setenv("DOCKER_CONTEXT", "")

	_, err := NewClient(WithClientBackend("docker"))
	if err == nil {
		t.Fatal("NewClient accepted a malformed DOCKER_HOST")
	}
	if strings.Contains(err.Error(), "daemon-secret") {
		t.Fatalf("environment Docker target leaked into construction error: %v", err)
	}
}

type failingContextRunner struct {
	detail string
	env    []string
}

func (r *failingContextRunner) ExternalEnv() []string {
	return r.env
}

func (r *failingContextRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	return nil, []byte(r.detail), &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: r.detail}
}

func TestPullRedactsDockerTargetValues(t *testing.T) {
	const contextName = "secret-context-name"
	t.Setenv(backendEnv, "docker")

	runner := &failingContextRunner{detail: "context " + contextName + ": context not found"}
	err := Pull(context.Background(), "redis:7-alpine",
		WithPullRunner(runner), WithPullDockerContext(contextName))
	if err == nil {
		t.Fatal("Pull accepted an unknown Docker context")
	}
	if strings.Contains(err.Error(), contextName) {
		t.Fatalf("Docker target leaked into pull error: %v", err)
	}
}

type blockingPullRunner struct {
	*fakeRunner
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (r *blockingPullRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "pull" {
		r.startOnce.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *blockingPullRunner) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func TestPullTimeoutBoundsWaitingOnSharedFlight(t *testing.T) {
	t.Setenv(backendEnv, "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	const (
		image = "redis:7-alpine"
		host  = "tcp://daemon-a:2375"
	)
	leader := &blockingPullRunner{
		fakeRunner: newDockerTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	t.Cleanup(leader.unblock)

	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- Pull(context.Background(), image,
			WithPullRunner(leader),
			WithPullDockerHost(host),
			WithPullTimeout(30*time.Second))
	}()

	select {
	case <-leader.started:
	case err := <-leaderDone:
		t.Fatalf("leader finished before pulling: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not start pulling")
	}

	follower := newDockerTestRunner()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	start := time.Now()
	err := Pull(waitCtx, image,
		WithPullRunner(follower),
		WithPullDockerHost(host),
		WithPullTimeout(100*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("follower error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("follower waited %v despite its 100ms timeout", elapsed)
	}

	leader.unblock()
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader: %v", err)
	}
}

func TestPinRunnerTargetEnvAnchorsPruneRunner(t *testing.T) {
	eng := newDockerEngine("tcp://daemon-a:2375", "", "", "")
	runner := &cli.ExecRunner{Binary: "docker"}

	pinned, ok := pinRunnerTargetEnv(eng, runner).(*cli.ExecRunner)
	if !ok {
		t.Fatal("pinRunnerTargetEnv did not return an ExecRunner")
	}
	found := false
	for _, entry := range pinned.Env {
		if entry == "DOCKER_HOST=tcp://daemon-a:2375" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prune runner was not pinned to the Client target: %q", pinned.Env)
	}
	if len(runner.Env) != 0 {
		t.Fatalf("pinRunnerTargetEnv mutated the caller runner: %q", runner.Env)
	}
}

type timeoutErrorPullRunner struct{}

func (r *timeoutErrorPullRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "pull" {
		<-ctx.Done()
		return nil, nil, fmt.Errorf("backend exploded")
	}
	return nil, nil, nil
}

func TestPullLeaderKeepsFetchErrorAfterTimeout(t *testing.T) {
	eng := newDockerEngine("tcp://daemon-a:2375", "", "", "")
	cfg := &config{
		runner:     &timeoutErrorPullRunner{},
		eng:        eng,
		runTimeout: 100 * time.Millisecond,
	}
	err := pullWithConfig(context.Background(), cfg, "redis:7-alpine")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "backend exploded") {
		t.Fatalf("leader dropped the fetch error: %v", err)
	}
}

func TestPullTimeoutNearMaxDurationDoesNotOverflow(t *testing.T) {
	t.Setenv(backendEnv, "docker")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	f := newDockerTestRunner()
	err := Pull(context.Background(), "redis:7-alpine",
		WithPullRunner(f),
		WithPullDockerHost("tcp://daemon-a:2375"),
		WithPullTimeout(time.Duration(math.MaxInt64)))
	if err != nil {
		t.Fatalf("Pull with a maximum timeout failed: %v", err)
	}
	if f.callWith("pull") == nil {
		t.Fatalf("pull was not called: %v", f.calls)
	}
}

func TestClientPruneParsesOptionsOnce(t *testing.T) {
	client, err := NewClient(
		WithClientBackend("docker"),
		WithClientDockerHost("tcp://daemon-a:2375"),
		WithClientRunner(&recordingRunner{}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	calls := 0
	opt := func(*pruneConfig) error {
		calls++
		return nil
	}
	if _, err := client.Prune(context.Background(), opt); err != nil {
		t.Fatalf("Client.Prune: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Client.Prune applied its options %d times, want 1", calls)
	}

	calls = 0
	if _, err := client.PruneReuseGroup(context.Background(), "test-group", opt); err != nil {
		t.Fatalf("Client.PruneReuseGroup: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Client.PruneReuseGroup applied its options %d times, want 1", calls)
	}
}

func TestNewClientRedactsRunnerDockerTarget(t *testing.T) {
	const contextName = "runner-secret-context"
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	runner := &failingContextRunner{
		detail: "context " + contextName + ": context not found",
		env:    []string{"DOCKER_CONTEXT=" + contextName},
	}
	_, err := NewClient(
		WithClientBackend("docker"),
		WithClientRunner(runner),
	)
	if err == nil {
		t.Fatal("NewClient accepted an unknown runner-supplied Docker context")
	}
	if strings.Contains(err.Error(), contextName) {
		t.Fatalf("runner Docker target leaked into construction error: %v", err)
	}
}

func TestPruneWithConfigDefaultsZeroValues(t *testing.T) {
	f := &lsRunner{fakeRunner: newTestRunner(), lsJSON: "[]"}
	removed, err := pruneWithConfig(context.Background(), f, appleEngine{}, pruneConfig{})
	if err != nil {
		t.Fatalf("pruneWithConfig: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want empty", removed)
	}
}
