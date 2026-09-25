package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestMain(m *testing.M) {
	// The developer's shell must not redirect fixture-backed tests to
	// another backend; tests opting in use t.Setenv.
	os.Unsetenv("CONTAINERGO_BACKEND")
	os.Exit(m.Run())
}

// fakeRunner records CLI calls and replays canned results.
type fakeRunner struct {
	mu          sync.Mutex
	calls       [][]string
	envFiles    []string // contents of --env-file captured at call time
	inspectJSON string
	failPrefix  string // fail calls whose first arg matches
	systemUp    bool

	imagePresent bool // image in the local store (image inspect/pull)
	pullCalls    int
	creations    map[string]string // container name -> creation generation from run args
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	for i, a := range args {
		if a == "--env-file" && i+1 < len(args) {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				return nil, nil, fmt.Errorf("read env-file: %w", err)
			}
			f.envFiles = append(f.envFiles, string(data))
		}
	}
	if args[0] == "system" {
		if f.systemUp {
			return []byte("running"), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1}
	}
	if args[0] == "version" || args[0] == "info" {
		if f.systemUp {
			return []byte("ok"), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}
	}
	// Image handling for both backends: docker inspects via
	// `image inspect` and pulls via `pull`; apple uses
	// `image inspect` / `image pull`.
	if args[0] == "image" && len(args) > 1 && args[1] == "inspect" {
		if !f.systemUp {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "XPC connection error"}
		}
		if f.imagePresent {
			return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
		}
		// The message carries both backends' not-found wording so one
		// fake serves the docker and apple classifiers.
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "image not found: redis:7-alpine (No such image)"}
	}
	if (args[0] == "image" && len(args) > 1 && args[1] == "pull") || args[0] == "pull" {
		if !f.systemUp {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "XPC connection error"}
		}
		f.imagePresent = true
		f.pullCalls++
		return nil, nil, nil
	}
	if f.failPrefix != "" && args[0] == f.failPrefix {
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected failure"}
	}
	switch args[0] {
	case "run":
		for i, a := range args {
			if a == "--label" && i+1 < len(args) {
				if v, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					name := ""
					for j, b := range args {
						if b == "--name" && j+1 < len(args) {
							name = args[j+1]
						}
					}
					if name != "" {
						if f.creations == nil {
							f.creations = map[string]string{}
						}
						f.creations[name] = v
					}
				}
			}
		}
		// Docker's detached run prints a full immutable ID. Apple
		// ignores stdout, so one stable fixture keeps both fake
		// backends useful without weakening Docker identity checks.
		return []byte(strings.Repeat("a", 64) + "\n"), nil, nil
	case "inspect":
		json := f.inspectJSON
		if json == "" {
			// Answer for whatever id was asked, echoing back the
			// creation generation captured at run time so
			// generation-verified deletes succeed.
			name := args[len(args)-1]
			json = fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "docker.io/library/redis:7-alpine"},
      "publishedPorts": [],
      "labels": {"com.github.hirokazumiyaji.container-go": "true", "com.github.hirokazumiyaji.container-go.session": %q, "com.github.hirokazumiyaji.container-go.creation": %q}
    },
    "status": {
      "state": "running",
      "networks": [{"ipv4Address": "192.168.64.3/24", "network": "default"}]
    }
  }
]`, name, name, sessionID(), f.creations[name])
		}
		return []byte(json), nil, nil
	default:
		return nil, nil, nil
	}
}

func (f *fakeRunner) callWith(subcommand string) []string {
	for _, c := range f.calls {
		if c[0] == subcommand {
			return c
		}
	}
	return nil
}

func newTestRunner() *fakeRunner {
	return &fakeRunner{systemUp: true}
}

func runTestContainer(t *testing.T, f cli.Runner, opts ...Option) *Container {
	t.Helper()
	// Pin the apple engine so a CONTAINERGO_BACKEND in the developer's
	// environment cannot redirect the apple-shaped fixtures.
	opts = append([]Option{WithName("myctr"), withRunner(f), withEngine(appleEngine{})}, opts...)
	ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return ctr
}

func TestRunInvokesRunDetachedWithImage(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	runCall := f.callWith("run")
	if runCall == nil {
		t.Fatal("no `run` call recorded")
	}
	if !slices.Contains(runCall, "--detach") {
		t.Errorf("run call missing --detach: %v", runCall)
	}
	if runCall[len(runCall)-1] != "redis:7-alpine" {
		t.Errorf("image not last arg: %v", runCall)
	}
	if ctr.ID() != "myctr" {
		t.Errorf("ID = %q", ctr.ID())
	}
}

func TestRunAppendsCmdAfterImage(t *testing.T) {
	f := newTestRunner()
	runTestContainer(t, f, WithCmd("redis-server", "--appendonly", "yes"))

	runCall := f.callWith("run")
	i := slices.Index(runCall, "redis:7-alpine")
	if i < 0 || !slices.Equal(runCall[i+1:], []string{"redis-server", "--appendonly", "yes"}) {
		t.Errorf("cmd not after image: %v", runCall)
	}
}

func TestRunGeneratesNameWhenUnset(t *testing.T) {
	f := newTestRunner()
	f.inspectJSON = "" // ignored; we only check the arg
	ctr, err := Run(context.Background(), "redis:7-alpine", withRunner(f))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(ctr.ID(), "containergo-") {
		t.Errorf("generated name = %q, want containergo- prefix", ctr.ID())
	}
	ctr2, err := Run(context.Background(), "redis:7-alpine", withRunner(f))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.ID() == ctr2.ID() {
		t.Error("two generated names are identical")
	}
}

func TestRunRejectsInvalidNameBeforeCLICall(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine", WithName("bad name; rm -rf /"), withRunner(f))
	if err == nil {
		t.Fatal("want error for invalid name")
	}
	if len(f.calls) != 0 {
		t.Errorf("CLI was called despite invalid name: %v", f.calls)
	}
}

func TestRunAddsSessionLabels(t *testing.T) {
	f := newTestRunner()
	runTestContainer(t, f)

	runCall := f.callWith("run")
	joined := strings.Join(runCall, " ")
	if !strings.Contains(joined, "--label com.github.hirokazumiyaji.container-go=true") {
		t.Errorf("missing managed label: %v", runCall)
	}
	if !strings.Contains(joined, "--label com.github.hirokazumiyaji.container-go.session=") {
		t.Errorf("missing session label: %v", runCall)
	}
}

func TestRunRejectsInvalidLabelKey(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithLabels(map[string]string{"Invalid_Key": "v"}), withRunner(f))
	if err == nil {
		t.Fatal("want error for invalid label key")
	}
	if len(f.calls) != 0 {
		t.Errorf("CLI was called despite invalid label: %v", f.calls)
	}
}

func TestRunPassesEnvViaEnvFileNotArgv(t *testing.T) {
	f := newTestRunner()
	runTestContainer(t, f, WithEnv(map[string]string{"PASSWORD": "s3cret"}))

	runCall := f.callWith("run")
	if slices.Contains(runCall, "--env") {
		t.Errorf("env passed via argv: %v", runCall)
	}
	if len(f.envFiles) != 1 {
		t.Fatalf("env file captures = %d, want 1", len(f.envFiles))
	}
	if !strings.Contains(f.envFiles[0], "PASSWORD=s3cret\n") {
		t.Errorf("env-file content = %q", f.envFiles[0])
	}
}

func TestRunRemovesEnvFileAfterStart(t *testing.T) {
	f := newTestRunner()
	runTestContainer(t, f, WithEnv(map[string]string{"A": "1"}))

	runCall := f.callWith("run")
	i := slices.Index(runCall, "--env-file")
	if i < 0 {
		t.Fatal("--env-file not passed")
	}
	if _, err := os.Stat(runCall[i+1]); !os.IsNotExist(err) {
		t.Errorf("env-file still exists after Run: %v", err)
	}
}

func TestRunRejectsInvalidEnvKey(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithEnv(map[string]string{"A=B": "v"}), withRunner(f))
	if err == nil {
		t.Fatal("want error for env key containing '='")
	}
	_, err = Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithEnv(map[string]string{"A": "line1\nline2"}), withRunner(f))
	if err == nil {
		t.Fatal("want error for env value containing newline")
	}
}

func TestRunPassesResourceAndProcessFlags(t *testing.T) {
	f := newTestRunner()
	runTestContainer(t, f,
		WithCPUs(2), WithMemory("512M"), WithUser("nobody"),
		WithWorkingDir("/work"), WithNetwork("mynet"), WithPlatform("linux/amd64"),
		WithEntrypoint("/entry.sh"))

	joined := strings.Join(f.callWith("run"), " ")
	for _, want := range []string{
		"--cpus 2", "--memory 512M", "--user nobody",
		"--workdir /work", "--network mynet", "--platform linux/amd64",
		"--entrypoint /entry.sh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args missing %q: %s", want, joined)
		}
	}
}

func TestRunPassesMounts(t *testing.T) {
	f := newTestRunner()
	runTestContainer(t, f,
		WithMounts(
			Mount{Type: MountBind, Source: "/host/data", Target: "/data", ReadOnly: true},
			Mount{Type: MountTmpfs, Target: "/scratch"},
		))

	joined := strings.Join(f.callWith("run"), " ")
	if !strings.Contains(joined, "--mount type=bind,source=/host/data,target=/data,readonly") {
		t.Errorf("bind mount missing: %s", joined)
	}
	if !strings.Contains(joined, "--mount type=tmpfs,target=/scratch") {
		t.Errorf("tmpfs mount missing: %s", joined)
	}
}

func TestRunRejectsMountWithComma(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine", WithName("myctr"),
		WithMounts(Mount{Type: MountBind, Source: "/a,b", Target: "/data"}), withRunner(f))
	if err == nil {
		t.Fatal("want error for comma in mount source")
	}
}

func TestRunSucceedsWithoutInitialInspect(t *testing.T) {
	f := newTestRunner()
	f.failPrefix = "inspect"
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.callWith("inspect") != nil {
		t.Errorf("inspect issued during Run: %v", f.calls)
	}
	if f.callWith("delete") != nil {
		t.Errorf("unexpected delete during Run: %v", f.calls)
	}
	if _, err := ctr.State(context.Background()); err == nil {
		t.Fatal("want error when first inspect fails after Run")
	}
}

func TestRunReportsSystemNotRunning(t *testing.T) {
	f := &fakeRunner{systemUp: false, failPrefix: "run"}
	_, err := Run(context.Background(), "redis:7-alpine", WithName("myctr"), withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
}

func TestStopPassesTimeout(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	d := 10 * time.Second
	if err := ctr.Stop(context.Background(), &d); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stop := f.callWith("stop")
	joined := strings.Join(stop, " ")
	if !strings.Contains(joined, "--time 10") || !slices.Contains(stop, "myctr") {
		t.Errorf("stop call = %v", stop)
	}
}

func TestTerminateIsIdempotent(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	// Second terminate: CLI reports not found; still success.
	f.failPrefix = "delete"
	f.calls = nil
	ferr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: `delete failed: not found: "myctr"`}
	f2 := &notFoundRunner{inner: f, err: ferr}
	ctr.runner = f2
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Errorf("Terminate on missing container = %v, want nil", err)
	}
}

type notFoundRunner struct {
	inner *fakeRunner
	err   error
}

func (n *notFoundRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "delete" {
		return nil, nil, n.err
	}
	return n.inner.Run(ctx, args...)
}

func TestStateReturnsFreshState(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	st, err := ctr.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != StateRunning {
		t.Errorf("State = %q, want %q", st, StateRunning)
	}
}

// --- Phase 5: connection info ---

func TestHostAndMappedPortDefaultToContainerIP(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("6379/tcp"))

	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "192.168.64.3" {
		t.Errorf("Host = %q, want container IP", host)
	}
	port, err := ctr.MappedPort(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("MappedPort: %v", err)
	}
	if port != 6379 {
		t.Errorf("MappedPort = %d, want 6379", port)
	}
	ep, err := ctr.Endpoint(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep != "192.168.64.3:6379" {
		t.Errorf("Endpoint = %q", ep)
	}
}

func TestMappedPortRejectsUndeclaredPort(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f, WithExposedPorts("6379/tcp"))

	_, err := ctr.MappedPort(context.Background(), "5432/tcp")
	if !errors.Is(err, ErrPortNotExposed) {
		t.Errorf("error = %v, want ErrPortNotExposed", err)
	}
}

func TestPublishedPortSwitchesToHostEndpoint(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f,
		WithExposedPorts("6379/tcp"),
		WithPublishedPort("127.0.0.1:16379:6379/tcp"))

	runCall := f.callWith("run")
	joined := strings.Join(runCall, " ")
	if !strings.Contains(joined, "--publish 127.0.0.1:16379:6379/tcp") {
		t.Errorf("publish flag missing: %s", joined)
	}

	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", host)
	}
	port, err := ctr.MappedPort(context.Background(), "6379/tcp")
	if err != nil {
		t.Fatalf("MappedPort: %v", err)
	}
	if port != 16379 {
		t.Errorf("MappedPort = %d, want 16379", port)
	}
}

func TestPublishedPortWithUnspecifiedHostReturnsLoopback(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f,
		WithExposedPorts("6379/tcp"),
		WithPublishedPort("16379:6379"))

	host, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", host)
	}
}

func TestPublishedPortParseErrors(t *testing.T) {
	f := newTestRunner()
	for _, spec := range []string{"", "abc", "70000:80", "8080:99999", "1:2:3:4:5"} {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithPublishedPort(spec), withRunner(f))
		if err == nil {
			t.Errorf("spec %q: want parse error", spec)
		}
	}
}

func TestContainerIP(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	ip, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("ContainerIP: %v", err)
	}
	if ip != "192.168.64.3" {
		t.Errorf("ContainerIP = %q", ip)
	}
}

func TestExposedPortParseErrors(t *testing.T) {
	f := newTestRunner()
	for _, spec := range []string{"", "0/tcp", "6379/sctp", "abc/tcp"} {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithExposedPorts(spec), withRunner(f))
		if err == nil {
			t.Errorf("spec %q: want parse error", spec)
		}
	}
}

func TestIsNotFoundRecognizesDockerWording(t *testing.T) {
	err := &cli.CLIError{Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "Error: No such object: myctr"}
	if !isNotFound(err) {
		t.Fatal("want isNotFound for docker 'No such object'")
	}
	// Matchers read Stderr, not Error(); Binary in the message must not matter.
	if !strings.Contains(err.Error(), "docker") {
		t.Fatalf("Error() = %q, want docker binary", err.Error())
	}
}
