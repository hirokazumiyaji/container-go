//go:build integration

package container_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/internal/integrationtest"
	"github.com/hirokazumiyaji/container-go/wait"
)

// requireDocker routes this test to the Docker backend, or skips/fails per
// backendPreflight.
func requireDocker(t *testing.T) {
	t.Helper()
	integrationtest.Preflight(t, "docker", integrationtest.DockerUnavailable)
}

func dockerServerOS(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Os}}").Output()
	if err != nil {
		t.Skipf("cannot determine Docker server OS: %v", err)
	}
	serverOS := strings.ToLower(strings.TrimSpace(string(out)))
	if serverOS == "" {
		t.Skip("Docker server did not report an OS")
	}
	return serverOS
}

func dockerHostNetworkUnavailable(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "host") &&
		(strings.Contains(message, "not supported") ||
			strings.Contains(message, "not enabled") ||
			strings.Contains(message, "unable to start") ||
			strings.Contains(message, "not available"))
}

func TestIntegrationDockerHostNetworkNoPublish(t *testing.T) {
	requireDocker(t)
	if serverOS := dockerServerOS(t); serverOS != "linux" {
		t.Skipf("host networking integration requires a Linux daemon, got %q", serverOS)
	}
	ctx := context.Background()
	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithNetwork("host"),
		container.WithCmd("sleep", "30"),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		if dockerHostNetworkUnavailable(err) {
			t.Skipf("Docker host networking is unavailable: %v", err)
		}
		t.Fatalf("host-mode Run: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("host-mode Host: %v", err)
	}
	if host == "" {
		t.Fatal("host-mode Host returned an empty address")
	}
	if _, err := ctr.Endpoint(ctx, "80/tcp"); !errors.Is(err, container.ErrPortNotExposed) {
		t.Fatalf("host-mode Endpoint error = %v, want ErrPortNotExposed", err)
	}
}

func TestIntegrationDockerHostAndNonePublishRejectedBeforeCreate(t *testing.T) {
	requireDocker(t)
	for _, network := range []string{"host", "none"} {
		t.Run(network, func(t *testing.T) {
			name := fmt.Sprintf("containergo-reject-%s-%d", network, time.Now().UnixNano())
			ctr, err := container.Run(context.Background(), integrationAlpine,
				container.WithName(name),
				container.WithNetwork(network),
				container.WithPublishedPort("127.0.0.1:18080:80/tcp"),
			)
			container.Cleanup(t, ctr)
			if !errors.Is(err, container.ErrInvalidConfig) {
				t.Fatalf("Run error = %v, want ErrInvalidConfig", err)
			}
			var configErr *container.ConfigError
			if !errors.As(err, &configErr) || configErr.Network != network {
				t.Fatalf("Run error = %v, want *ConfigError for %s", err, network)
			}
			if out, inspectErr := exec.Command("docker", "container", "inspect", name).CombinedOutput(); inspectErr == nil {
				t.Fatalf("container was created despite %s publish rejection: %s", network, out)
			}
		})
	}
}

func TestIntegrationDockerNoneNetwork(t *testing.T) {
	requireDocker(t)
	if serverOS := dockerServerOS(t); serverOS != "linux" {
		t.Skipf("none-network integration requires a Linux daemon, got %q", serverOS)
	}
	ctx := context.Background()
	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithNetwork("none"),
		container.WithCmd("sleep", "30"),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("none-mode Run: %v", err)
	}
	if _, err := ctr.Host(ctx); !errors.Is(err, container.ErrNoReachableHost) {
		t.Fatalf("none-mode Host error = %v, want ErrNoReachableHost", err)
	}
}

func TestIntegrationDockerWindowsNATDefault(t *testing.T) {
	requireDocker(t)
	if runtime.GOOS != "windows" {
		t.Skip("native Windows Docker integration")
	}
	if serverOS := dockerServerOS(t); serverOS != "windows" {
		t.Skipf("Windows NAT integration requires a Windows daemon, got %q", serverOS)
	}
	image := os.Getenv("CONTAINERGO_WINDOWS_TEST_IMAGE")
	if image == "" {
		t.Skip("set CONTAINERGO_WINDOWS_TEST_IMAGE to a small Windows image")
	}
	ctx := context.Background()
	ctr, err := container.Run(ctx, image,
		container.WithNetwork("nat"),
		container.WithCmd("cmd", "/C", "ping -n 10 127.0.0.1 >NUL"),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Windows NAT Run: %v", err)
	}
	if host, err := ctr.Host(ctx); err != nil || host == "" {
		t.Fatalf("Windows NAT Host = %q, err = %v", host, err)
	}
}

func TestIntegrationDockerRedisLifecycle(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()

	ctr, err := container.Run(ctx, integrationRedis,
		container.WithExposedPorts("6379/tcp"),
		container.WithWaitStrategy(wait.ForAll(
			wait.ForLog("Ready to accept connections"),
			wait.ForListeningPort("6379/tcp"),
		)),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Endpoints resolve to a daemon-assigned loopback port.
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", host)
	}
	endpoint, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	conn.Close()

	code, out, err := ctr.Exec(ctx, []string{"redis-cli", "ping"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, _ := io.ReadAll(out)
	if code != 0 || !strings.Contains(string(data), "PONG") {
		t.Errorf("redis-cli ping: code=%d out=%q", code, data)
	}

	src := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(src, []byte("hello docker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(ctx, src, "/tmp/hello.txt"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	rc, err := ctr.CopyFileFromContainer(ctx, "/tmp/hello.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	defer rc.Close()
	round, _ := io.ReadAll(rc)
	if string(round) != "hello docker" {
		t.Errorf("round-tripped content = %q", round)
	}

	logs, err := ctr.Logs(ctx)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer logs.Close()
	logData, _ := io.ReadAll(logs)
	if !strings.Contains(string(logData), "Ready to accept connections") {
		t.Errorf("logs missing readiness line: %q", logData)
	}

	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := ctr.State(ctx); err == nil {
		t.Error("State after Terminate: want error, got nil")
	}
}

func TestIntegrationDockerStopTimeoutRoundsUp(t *testing.T) {
	requireDocker(t)
	runStopTimingIntegration(t, "docker", 1500*time.Millisecond, 1400*time.Millisecond, 10*time.Second)
}

func TestIntegrationDockerStopTimeoutZeroIsImmediate(t *testing.T) {
	requireDocker(t)
	runStopTimingIntegration(t, "docker-zero", 0, 0, dockerStopImmediateMaxElapsed)
func TestIntegrationDockerRejectsInternalNetworkEndpoints(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	network := fmt.Sprintf("containergo-internal-%d", os.Getpid())
	if out, err := exec.Command("docker", "network", "create", "--internal", network).CombinedOutput(); err != nil {
		t.Fatalf("docker network create --internal: %v: %s", err, out)
	}
	defer func() {
		if out, err := exec.Command("docker", "network", "rm", network).CombinedOutput(); err != nil {
			t.Errorf("docker network rm %s: %v: %s", network, err, out)
		}
	}()

	for i, option := range []struct {
		name string
		opt  container.Option
	}{
		{name: "exposed", opt: container.WithExposedPorts("6379/tcp")},
		{name: "published", opt: container.WithPublishedPort("127.0.0.1:18080:6379/tcp")},
	} {
		t.Run(option.name, func(t *testing.T) {
			name := fmt.Sprintf("containergo-internal-ctr-%d-%d", os.Getpid(), i)
			ctr, err := container.Run(ctx, integrationRedis,
				container.WithName(name),
				container.WithNetwork(network),
				option.opt,
			)
			container.Cleanup(t, ctr)
			if err == nil {
				t.Fatal("Run succeeded; want typed isolated-network configuration error")
			}
			if !errors.Is(err, container.ErrInvalidConfig) {
				t.Fatalf("Run error = %v, want ErrInvalidConfig", err)
			}
			var configErr *container.ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("Run error = %v, want *ConfigError", err)
			}
			if configErr.Backend != "docker" || configErr.Network != network {
				t.Errorf("ConfigError = %+v, want Docker network %q", configErr, network)
			}
			if out, inspectErr := exec.Command("docker", "container", "inspect", name).CombinedOutput(); inspectErr == nil {
				t.Errorf("container was created despite configuration rejection: %s", out)
			}
		})
	}
}

func TestIntegrationDockerIPv6PublishedEndpoint(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	// Expanded loopback input must canonicalize without changing family.
	spec := fmt.Sprintf("[0:0:0:0:0:0:0:1]:%d:6379/tcp", port)
	ctr, err := container.Run(ctx, integrationRedis,
		container.WithExposedPorts("6379/tcp"),
		container.WithPublishedPort(spec),
		container.WithWaitStrategy(wait.ForListeningPort("6379/tcp").WithStartupTimeout(2*time.Minute)),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil || host != "::1" {
		t.Fatalf("Host = %q, err = %v; want ::1", host, err)
	}
	endpoint, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != fmt.Sprintf("[::1]:%d", port) {
		t.Fatalf("Endpoint = %q, want [::1]:%d", endpoint, port)
	}
	conn, err := net.DialTimeout("tcp6", endpoint, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	_ = conn.Close()
}

func TestIntegrationDockerParallelStarts(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	const n = 5

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctr, err := container.Run(ctx, integrationAlpine,
				container.WithCmd("sleep", "60"))
			container.Cleanup(t, ctr)
			if err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("container %d: %v", i, err)
		}
	}
}

func TestIntegrationDockerReaperSurvivesSIGKILL(t *testing.T) {
	if os.Getenv("CONTAINERGO_REAPER_CHILD") == "1" {
		// The child's TestMain unsets CONTAINERGO_BACKEND, so the
		// env-passed selection never reaches Run; pin it here.
		os.Setenv("CONTAINERGO_BACKEND", "docker")
		ctx := context.Background()
		ctr, err := container.Run(ctx, integrationAlpine,
			container.WithName(os.Getenv("CONTAINERGO_REAPER_NAME")),
			container.WithCmd("sleep", "120"))
		if err != nil {
			fmt.Println("CHILD-ERROR:", err)
			os.Exit(1)
		}
		fmt.Println("READY:", ctr.ID())
		select {}
	}

	requireDocker(t)
	name := fmt.Sprintf("containergo-dockerreap-%d", os.Getpid())

	cmd := exec.Command(os.Args[0], "-test.run", "TestIntegrationDockerReaperSurvivesSIGKILL")
	cmd.Env = append(os.Environ(),
		"CONTAINERGO_BACKEND=docker",
		"CONTAINERGO_REAPER_CHILD=1",
		"CONTAINERGO_REAPER_NAME="+name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "--force", name).Run()
	}()

	ready := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := stdout.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "READY:") || err != nil {
				ready <- acc
				return
			}
		}
	}()
	select {
	case out := <-ready:
		if !strings.Contains(out, "READY:") {
			t.Fatalf("child failed before starting container: %q", out)
		}
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("child never became ready")
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "ps", "--all", "--format", "{{.Names}}").Output()
		if err == nil && !strings.Contains(string(out), name) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("container %s still present 60s after SIGKILL; reaper did not fire", name)
}

// TestIntegrationDockerLazyInspectStateAndWaitRollback covers #20 for
// the Docker backend: deferred first inspect still exposes State, and a
// failing connection wait rolls the container back.
func TestIntegrationDockerLazyInspectStateAndWaitRollback(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()

	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithCmd("sleep", "60"),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	st, err := ctr.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != container.StateRunning {
		t.Fatalf("State = %q, want %q", st, container.StateRunning)
	}
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	name := fmt.Sprintf("containergo-lazy-%d", os.Getpid())
	_, err = container.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithExposedPorts("80/tcp"),
		container.WithCmd("sleep", "60"),
		container.WithWaitStrategy(
			wait.ForHTTP("/").
				WithStartupTimeout(3*time.Second).
				WithPollInterval(200*time.Millisecond),
		),
	)
	if err == nil {
		t.Fatal("want error when HTTP wait cannot succeed")
	}
	if out, inspectErr := exec.Command("docker", "inspect", name).CombinedOutput(); inspectErr == nil {
		t.Fatalf("container still present after wait rollback: %s", out)
	}
}

// TestIntegrationDockerReuseSharedAcrossProcesses starts two helper
// processes that WithReuse the same name and checks they share one
// container.
func TestIntegrationDockerReuseSharedAcrossProcesses(t *testing.T) {
	// Scoped per run so two concurrent runs of this suite cannot prune or
	// delete each other's containers.
	group := os.Getenv("CONTAINERGO_REUSE_GROUP")
	if group == "" {
		group = "integration-reuse-" + integrationtest.Nonce()
	}

	if os.Getenv("CONTAINERGO_REUSE_CHILD") == "1" {
		requireDocker(t)
		ctx := context.Background()
		ctr, err := container.Run(ctx, integrationRedis,
			container.WithName(os.Getenv("CONTAINERGO_REUSE_NAME")),
			container.WithReuse(),
			container.WithReuseGroup(group),
			container.WithExposedPorts("6379/tcp"),
			container.WithWaitStrategy(wait.ForListeningPort("6379/tcp").WithStartupTimeout(2*time.Minute)),
		)
		if err != nil {
			fmt.Println("CHILD-ERROR:", err)
			os.Exit(1)
		}
		st, err := ctr.State(ctx)
		if err != nil {
			fmt.Println("CHILD-ERROR:", err)
			os.Exit(1)
		}
		fmt.Println("READY:", ctr.ID(), st)
		select {}
	}

	requireDocker(t)
	name := fmt.Sprintf("containergo-reuse-%d-%s", os.Getpid(), integrationtest.Nonce())
	defer func() {
		// A teardown failure must not be discarded: a leaked container
		// otherwise survives a green run unnoticed.
		if _, err := container.PruneReuseGroup(context.Background(), group); err != nil {
			t.Errorf("prune reuse group %s: %v", group, err)
		}
	}()

	startChild := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestIntegrationDockerReuseSharedAcrossProcesses$")
		cmd.Env = append(os.Environ(),
			"CONTAINERGO_BACKEND=docker",
			"CONTAINERGO_REUSE_CHILD=1",
			"CONTAINERGO_REUSE_GROUP="+group,
			"CONTAINERGO_REUSE_NAME="+name)
		return cmd
	}

	c1, c2 := startChild(), startChild()
	out1, err1 := c1.StdoutPipe()
	if err1 != nil {
		t.Fatal(err1)
	}
	out2, err2 := c2.StdoutPipe()
	if err2 != nil {
		t.Fatal(err2)
	}
	// Track each child as it starts, so a failure on the second start still
	// reaps the first.
	children := &integrationtest.ChildGroup{}
	defer children.Kill()
	if err := c1.Start(); err != nil {
		t.Fatal(err)
	}
	children.Track(c1)
	if err := c2.Start(); err != nil {
		t.Fatal(err)
	}
	children.Track(c2)

	// The partial output is reported with the error: a hang is exactly when
	// it is the only diagnostic available.
	o1, err := integrationtest.ReadReady(out1, integrationtest.ChildReadyTimeout)
	if err != nil {
		t.Fatalf("child1: %v (output so far: %q)", err, o1)
	}
	o2, err := integrationtest.ReadReady(out2, integrationtest.ChildReadyTimeout)
	if err != nil {
		t.Fatalf("child2: %v (output so far: %q)", err, o2)
	}
	if !strings.Contains(o1, "READY:") {
		t.Fatalf("child1: %q", o1)
	}
	if !strings.Contains(o2, "READY:") {
		t.Fatalf("child2: %q", o2)
	}
	if !strings.Contains(o1, name) || !strings.Contains(o2, name) {
		t.Fatalf("children did not share name %s: %q / %q", name, o1, o2)
	}

	out, err := exec.Command("docker", "ps", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 1 {
		t.Fatalf("running containers named %s: %v, want 1", name, lines)
	}
}

// TestIntegrationDockerRunFailureCleansUp covers #48: a failed start
// must not leave a created container behind.
func TestIntegrationDockerRunFailureCleansUp(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	name := fmt.Sprintf("containergo-failclean-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "--force", name).Run()

	_, err := container.Run(ctx, integrationRedis,
		container.WithName(name),
		container.WithPullPolicy(container.PullNever),
		container.WithEntrypoint("/does-not-exist-audit"),
	)
	if err == nil {
		_ = exec.Command("docker", "rm", "--force", name).Run()
		t.Fatal("want error for bad entrypoint")
	}
	if out, inspectErr := exec.Command("docker", "inspect", name).CombinedOutput(); inspectErr == nil {
		_ = exec.Command("docker", "rm", "--force", name).Run()
		t.Fatalf("container still present after failed Run: %s", out)
	}
}

// TestIntegrationDockerRunFailurePreservesConflict covers #48: a name
// conflict must not delete the pre-existing container.
func TestIntegrationDockerRunFailurePreservesConflict(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	name := fmt.Sprintf("containergo-failkeep-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "--force", name).Run()

	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	defer func() {
		_ = ctr.Terminate(context.Background())
	}()

	_, err = container.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithCmd("sleep", "60"),
	)
	if err == nil {
		t.Fatal("want conflict error for duplicate name")
	}
	if out, inspectErr := exec.Command("docker", "inspect", name).CombinedOutput(); inspectErr != nil {
		t.Fatalf("existing container missing after conflict: %s / %v", out, inspectErr)
	}
}

// TestIntegrationDockerStaleHandlePreservesReplacement covers #49: an
// old handle must not delete a same-name replacement.
func TestIntegrationDockerStaleHandlePreservesReplacement(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	name := fmt.Sprintf("containergo-stale-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "--force", name).Run()
	defer func() {
		_ = exec.Command("docker", "rm", "--force", name).Run()
	}()

	oldCtr, err := container.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := oldCtr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate old: %v", err)
	}
	newCtr, err := container.Run(ctx, integrationAlpine,
		container.WithName(name),
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	defer func() {
		_ = newCtr.Terminate(context.Background())
	}()
	// Docker's stale handle retains the old immutable ID. A second
	// termination is therefore an idempotent no-op, while the
	// same-name replacement must survive.
	if err := oldCtr.Terminate(ctx); err != nil {
		t.Fatalf("stale Docker Terminate = %v, want idempotent success", err)
	}
	if out, inspectErr := exec.Command("docker", "inspect", name).CombinedOutput(); inspectErr != nil {
		t.Fatalf("replacement missing after stale Terminate: %s / %v", out, inspectErr)
	}
}

// TestIntegrationDockerExecPreservesLargeStderr covers #52: success
// output must not be truncated at 64 KiB.
func TestIntegrationDockerExecPreservesLargeStderr(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	ctr, err := container.Run(ctx, integrationRedis,
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = ctr.Terminate(context.Background())
	}()
	code, out, err := ctr.Exec(ctx, []string{"sh", "-c", "head -c 131072 /dev/zero >&2"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if n, _ := io.ReadAll(out); len(n) != 131072 {
		t.Fatalf("len(output) = %d, want 131072", len(n))
	}
}

// TestIntegrationDockerExecPreservesLargeFailureOutput covers #52 for
// non-zero exits.
func TestIntegrationDockerExecPreservesLargeFailureOutput(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	ctr, err := container.Run(ctx, integrationRedis,
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = ctr.Terminate(context.Background())
	}()
	code, out, err := ctr.Exec(ctx, []string{"sh", "-c", "head -c 131072 /dev/zero >&2; exit 7"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
	if n, _ := io.ReadAll(out); len(n) != 131072 {
		t.Fatalf("len(output) = %d, want 131072", len(n))
	}
}

// TestIntegrationDockerExecAppNotFoundIsResult covers #53: app stderr
// containing "not found" must not be mistaken for a missing container.
func TestIntegrationDockerExecAppNotFoundIsResult(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = ctr.Terminate(context.Background())
	}()
	code, _, err := ctr.Exec(ctx, []string{"sh", "-c", "echo 'record not found' >&2; exit 7"})
	if err != nil {
		t.Fatalf("Exec: %v, want app result", err)
	}
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
}

// TestIntegrationDockerExecMissingContainerIsError covers #53: exec on
// a removed container must fail.
func TestIntegrationDockerExecMissingContainerIsError(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	ctr, err := container.Run(ctx, integrationAlpine,
		container.WithCmd("sleep", "60"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, _, err := ctr.Exec(ctx, []string{"true"}); err == nil {
		t.Fatal("want error for exec on missing container")
	}
}
