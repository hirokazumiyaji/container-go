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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

// requireDocker skips unless the docker CLI and daemon are available,
// and routes this test to the Docker backend. When CONTAINERGO_BACKEND
// is set to a non-docker value, Docker integration tests are skipped.
func requireDocker(t *testing.T) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "docker" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Docker integration", backend)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	t.Setenv("CONTAINERGO_BACKEND", "docker")
}

func dockerMountVolumeName(t *testing.T, containerName, destination string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format",
		fmt.Sprintf(`{{range .Mounts}}{{if eq .Destination %q}}{{.Name}}{{end}}{{end}}`, destination),
		containerName,
	).Output()
	if err != nil {
		t.Fatalf("inspect mounts for %s: %v", containerName, err)
	}
	return strings.TrimSpace(string(out))
}

func dockerContainerID(t *testing.T, containerName string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", containerName).Output()
	if err != nil {
		t.Fatalf("inspect ID for %s: %v", containerName, err)
	}
	return strings.TrimSpace(string(out))
}

const (
	dockerVolumeWaitTimeout  = 10 * time.Second
	dockerVolumeProbeTimeout = 2 * time.Second
)

func inspectDockerVolumeWith(ctx context.Context, run func(context.Context, ...string) ([]byte, error), name string) (bool, error) {
	out, err := run(ctx, "volume", "inspect", name)
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("inspect Docker volume %q: %w: %s", name, ctxErr, strings.TrimSpace(string(out)))
	}
	if strings.Contains(strings.ToLower(string(out)), "no such volume") {
		return false, nil
	}
	return false, fmt.Errorf("inspect Docker volume %q: %w: %s", name, err, strings.TrimSpace(string(out)))
}

func dockerVolumeExists(ctx context.Context, name string) (bool, error) {
	return inspectDockerVolumeWith(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}, name)
}

func waitForDockerVolume(t *testing.T, name string, wantExists bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dockerVolumeWaitTimeout)
	defer cancel()
	lastExists := !wantExists
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, dockerVolumeProbeTimeout)
		exists, err := dockerVolumeExists(probeCtx, name)
		probeCancel()
		if err != nil {
			t.Fatalf("inspect volume %s: %v", name, err)
		}
		lastExists = exists
		if exists == wantExists {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("volume %s existence remained %v, want %v after %s", name, lastExists, wantExists, dockerVolumeWaitTimeout)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type captureVolumeThenFail struct {
	t      *testing.T
	name   string
	volume string
}

func (s *captureVolumeThenFail) WaitUntilReady(ctx context.Context, target wait.Target) error {
	// Force the same lazy inspect path as the HTTP strategy, then record the
	// image-defined volume while the container still exists.
	_, _ = target.Endpoint(ctx, "80/tcp")
	s.volume = dockerMountVolumeName(s.t, s.name, "/data")
	if s.volume == "" {
		return errors.New("image-defined /data volume not found")
	}
	waitForDockerVolume(s.t, s.volume, true)
	return errors.New("forced readiness failure")
}

func TestDockerVolumeInspectClassifiesOnlyNoSuchVolumeAsAbsent(t *testing.T) {
	exitErr := errors.New("exit status 1")
	for _, tc := range []struct {
		name        string
		out         string
		err         error
		wantExists  bool
		wantErrText string
	}{
		{name: "present", out: "[]", wantExists: true},
		{name: "absent", out: "Error response from daemon: get missing: no such volume\n", err: exitErr},
		{name: "daemon error", out: "Cannot connect to the Docker daemon\n", err: exitErr, wantErrText: "Cannot connect"},
		{name: "permission error", out: "permission denied while trying to connect to the Docker daemon socket\n", err: exitErr, wantErrText: "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			exists, err := inspectDockerVolumeWith(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
				gotArgs = append([]string(nil), args...)
				return []byte(tc.out), tc.err
			}, "volume-under-test")
			if exists != tc.wantExists {
				t.Errorf("exists = %v, want %v", exists, tc.wantExists)
			}
			if tc.wantErrText == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrText)) {
				t.Fatalf("error = %v, want text %q", err, tc.wantErrText)
			}
			if want := []string{"volume", "inspect", "volume-under-test"}; !slices.Equal(gotArgs, want) {
				t.Errorf("args = %v, want %v", gotArgs, want)
			}
		})
	}

	t.Run("context error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exists, err := inspectDockerVolumeWith(ctx, func(ctx context.Context, _ ...string) ([]byte, error) {
			return []byte("no such volume"), ctx.Err()
		}, "volume-under-test")
		if exists {
			t.Error("exists = true, want false")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
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

// The Redis image declares VOLUME /data, so this exercises a volume
// created by the image rather than an explicit anonymous mount.
func TestIntegrationDockerCleanupVolumePolicy(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	name := fmt.Sprintf("containergo-volume-%d", os.Getpid())

	ctr, err := container.Run(ctx, integrationRedis,
		container.WithName(name),
		container.WithEntrypoint("/bin/sh"),
		container.WithCmd("-c", "sleep 60"),
	)
	if err != nil {
		t.Fatalf("Run with image-defined volume: %v", err)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
	}()

	anonymous := dockerMountVolumeName(t, name, "/data")
	if anonymous == "" {
		t.Fatal("image-defined /data volume not found")
	}
	waitForDockerVolume(t, anonymous, true)
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	waitForDockerVolume(t, anonymous, false)

	named := name + "-named"
	if out, err := exec.Command("docker", "volume", "create", named).CombinedOutput(); err != nil {
		t.Fatalf("create named volume: %v: %s", err, out)
	}
	defer func() {
		_ = exec.Command("docker", "volume", "rm", named).Run()
	}()

	namedContainer := name + "-named-ctr"
	namedCtr, err := container.Run(ctx, integrationRedis,
		container.WithName(namedContainer),
		container.WithEntrypoint("/bin/sh"),
		container.WithCmd("-c", "sleep 60"),
		container.WithMounts(container.Mount{
			Type:   container.MountVolume,
			Source: named,
			Target: "/data",
		}),
	)
	if err != nil {
		_ = exec.Command("docker", "rm", "--force", "--volumes", namedContainer).Run()
		t.Fatalf("Run with named volume: %v", err)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", namedContainer).Run()
	}()

	if mounted := dockerMountVolumeName(t, namedContainer, "/data"); mounted != named {
		t.Fatalf("mounted volume = %q, want %q", mounted, named)
	}
	if err := namedCtr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate with named volume: %v", err)
	}
	waitForDockerVolume(t, named, true)
}

// TestIntegrationDockerNamedCustomDriverVolumePolicy is opt-in because
// Docker does not ship a third-party volume plugin. Set
// CONTAINERGO_DOCKER_VOLUME_DRIVER to an installed plugin alias to run it.
func TestIntegrationDockerNamedCustomDriverVolumePolicy(t *testing.T) {
	requireDocker(t)
	driver := os.Getenv("CONTAINERGO_DOCKER_VOLUME_DRIVER")
	if driver == "" {
		t.Skip("set CONTAINERGO_DOCKER_VOLUME_DRIVER to an installed Docker volume plugin")
	}
	ctx := context.Background()
	name := fmt.Sprintf("containergo-custom-volume-%d-%d", os.Getpid(), time.Now().UnixNano())
	named := name + "-named"
	if out, err := exec.Command("docker", "volume", "create", "--driver", driver, named).CombinedOutput(); err != nil {
		t.Skipf("custom volume driver %q is unavailable: %v: %s", driver, err, out)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
		_ = exec.Command("docker", "volume", "rm", "--force", named).Run()
	}()

	ctr, err := container.Run(ctx, integrationRedis,
		container.WithName(name),
		container.WithEntrypoint("sleep"),
		container.WithCmd("60"),
		container.WithMounts(container.Mount{
			Type:   container.MountVolume,
			Source: named,
			Target: "/custom",
		}),
	)
	if err != nil {
		t.Fatalf("Run with custom-driver volume: %v", err)
	}
	if mounted := dockerMountVolumeName(t, name, "/custom"); mounted != named {
		t.Fatalf("custom volume mount = %q, want %q", mounted, named)
	}
	anonymous := dockerMountVolumeName(t, name, "/data")
	if anonymous == "" {
		t.Fatal("image-defined /data volume not found")
	}
	waitForDockerVolume(t, anonymous, true)
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate custom-driver volume: %v", err)
	}
	waitForDockerVolume(t, anonymous, false)
	waitForDockerVolume(t, named, true)
}

func runDockerCommand(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func dockerContainerState(t *testing.T, name string) (string, bool) {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", name).CombinedOutput()
	if err == nil {
		state := strings.TrimSpace(string(out))
		if state == "" {
			t.Fatalf("docker inspect %s returned an empty state", name)
		}
		return state, true
	}
	message := strings.ToLower(string(out))
	if strings.Contains(message, "no such object") || strings.Contains(message, "no such container") {
		return "", false
	}
	t.Fatalf("docker inspect %s: %v: %s", name, err, out)
	return "", false
}

func waitDockerContainerState(t *testing.T, name, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, found := dockerContainerState(t, name)
		if found && state == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	state, found := dockerContainerState(t, name)
	t.Fatalf("state of %s = %q (found=%t), want %q", name, state, found, want)
}

func TestIntegrationDockerPruneKeepsCreatedAndRunning(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	if err := container.Pull(ctx, integrationAlpine); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	createdName := "containergo-prune-created-" + suffix
	exitedName := "containergo-prune-exited-" + suffix
	runningName := "containergo-prune-running-" + suffix
	names := []string{createdName, exitedName, runningName}
	t.Cleanup(func() {
		for _, name := range names {
			_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
		}
	})
	const managedLabel = "com.github.hirokazumiyaji.container-go=true"

	runDockerCommand(t, "create", "--label", managedLabel, "--name", createdName, integrationAlpine)
	runDockerCommand(t, "create", "--label", managedLabel, "--name", exitedName, integrationAlpine, "true")
	runDockerCommand(t, "start", exitedName)
	runDockerCommand(t, "run", "--detach", "--label", managedLabel, "--name", runningName,
		integrationAlpine, "sh", "-c", "while :; do sleep 3600; done")
	waitDockerContainerState(t, createdName, "created")
	waitDockerContainerState(t, exitedName, "exited")
	waitDockerContainerState(t, runningName, "running")

	if _, err := container.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if state, found := dockerContainerState(t, exitedName); found {
		t.Fatalf("exited container %s survived Prune (state=%q)", exitedName, state)
	}
	for name, want := range map[string]string{createdName: "created", runningName: "running"} {
		state, found := dockerContainerState(t, name)
		if !found {
			t.Errorf("Prune removed preserved container %s", name)
			continue
		}
		if state != want {
			t.Errorf("state of %s = %q, want %q", name, state, want)
		}
	}
}

func TestIntegrationDockerPruneAcceptsDeadStatus(t *testing.T) {
	requireDocker(t)
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "status=dead").CombinedOutput()
	if err != nil {
		t.Fatalf("Docker rejected status=dead: %v: %s", err, out)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		t.Skip("Docker accepts status=dead, but no dead container is available to inspect")
	}
	for _, id := range ids {
		out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", id).CombinedOutput()
		if err != nil {
			if strings.Contains(strings.ToLower(string(out)), "no such") {
				continue
			}
			t.Fatalf("inspect %s: %v: %s", id, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != "dead" {
			t.Errorf("status=dead returned %s with state %q", id, got)
		}
	}
}

func TestIntegrationDockerPruneVolumePolicies(t *testing.T) {
	// These checks use Docker's built-in named-volume driver. A custom
	// driver is not assumed when no plugin is installed.
	requireDocker(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		reuse bool
	}{
		{name: "prune"},
		{name: "prune-reuse-group", reuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("containergo-%s-%d", tc.name, os.Getpid())
			named := name + "-named"
			if out, err := exec.Command("docker", "volume", "create", named).CombinedOutput(); err != nil {
				t.Fatalf("create named volume: %v: %s", err, out)
			}
			defer func() {
				_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
				_ = exec.Command("docker", "volume", "rm", named).Run()
			}()

			opts := []container.Option{
				container.WithName(name),
				container.WithEntrypoint("sleep"),
				container.WithCmd("60"),
				container.WithMounts(container.Mount{
					Type:   container.MountVolume,
					Source: named,
					Target: "/named",
				}),
			}
			group := ""
			if tc.reuse {
				group = fmt.Sprintf("volume-policy-%d", os.Getpid())
				opts = append(opts, container.WithReuse(), container.WithReuseGroup(group))
			}
			ctr, err := container.Run(ctx, integrationRedis, opts...)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if mounted := dockerMountVolumeName(t, name, "/named"); mounted != named {
				t.Fatalf("named mount = %q, want %q", mounted, named)
			}
			containerID := dockerContainerID(t, name)
			anonymous := dockerMountVolumeName(t, name, "/data")
			if anonymous == "" {
				t.Fatal("image-defined /data volume not found")
			}
			waitForDockerVolume(t, anonymous, true)
			waitForDockerVolume(t, named, true)

			if err := ctr.Stop(ctx, nil); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			var removed []string
			if tc.reuse {
				removed, err = container.PruneReuseGroup(ctx, group)
			} else {
				removed, err = container.Prune(ctx)
			}
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			found := slices.ContainsFunc(removed, func(id string) bool {
				return id == containerID || strings.HasPrefix(containerID, id) || strings.HasPrefix(id, containerID)
			})
			if !found {
				t.Fatalf("removed = %v, want container %s", removed, containerID)
			}
			waitForDockerVolume(t, anonymous, false)
			waitForDockerVolume(t, named, true)
		})
	}
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
	named := name + "-named"
	if out, err := exec.Command("docker", "volume", "create", named).CombinedOutput(); err != nil {
		t.Fatalf("create named volume: %v: %s", err, out)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
		_ = exec.Command("docker", "volume", "rm", named).Run()
	}()
	strategy := &captureVolumeThenFail{t: t, name: name}
	_, err = container.Run(ctx, integrationRedis,
		container.WithName(name),
		container.WithEntrypoint("sleep"),
		container.WithCmd("60"),
		container.WithExposedPorts("80/tcp"),
		container.WithMounts(container.Mount{
			Type:   container.MountVolume,
			Source: named,
			Target: "/named",
		}),
		container.WithWaitStrategy(strategy),
	)
	if err == nil {
		t.Fatal("want error when readiness wait fails")
	}
	if out, inspectErr := exec.Command("docker", "inspect", name).CombinedOutput(); inspectErr == nil {
		t.Fatalf("container still present after wait rollback: %s", out)
	}
	if strategy.volume == "" {
		t.Fatal("readiness strategy did not capture the anonymous volume")
	}
	waitForDockerVolume(t, strategy.volume, false)
	waitForDockerVolume(t, named, true)
}

// TestIntegrationDockerReuseSharedAcrossProcesses starts two helper
// processes that WithReuse the same name and checks they share one
// container.
func TestIntegrationDockerReuseSharedAcrossProcesses(t *testing.T) {
	if os.Getenv("CONTAINERGO_REUSE_CHILD") == "1" {
		requireDocker(t)
		ctx := context.Background()
		ctr, err := container.Run(ctx, integrationRedis,
			container.WithName(os.Getenv("CONTAINERGO_REUSE_NAME")),
			container.WithReuse(),
			container.WithReuseGroup("integration-reuse"),
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
	name := fmt.Sprintf("containergo-reuse-%d", os.Getpid())
	defer func() {
		_, _ = container.PruneReuseGroup(context.Background(), "integration-reuse")
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
	}()

	startChild := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestIntegrationDockerReuseSharedAcrossProcesses$")
		cmd.Env = append(os.Environ(),
			"CONTAINERGO_BACKEND=docker",
			"CONTAINERGO_REUSE_CHILD=1",
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
	if err := c1.Start(); err != nil {
		t.Fatal(err)
	}
	if err := c2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = c1.Process.Kill()
		_ = c2.Process.Kill()
		_, _ = c1.Process.Wait()
		_, _ = c2.Process.Wait()
	}()

	readReady := func(r io.Reader) string {
		buf := make([]byte, 4096)
		var acc string
		deadline := time.After(3 * time.Minute)
		for {
			select {
			case <-deadline:
				return acc
			default:
			}
			n, err := r.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "READY:") || strings.Contains(acc, "CHILD-ERROR:") || err != nil {
				return acc
			}
		}
	}

	o1 := readReady(out1)
	o2 := readReady(out2)
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
	_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
	named := name + "-named"
	if out, err := exec.Command("docker", "volume", "create", named).CombinedOutput(); err != nil {
		t.Fatalf("create named volume: %v: %s", err, out)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
		_ = exec.Command("docker", "volume", "rm", named).Run()
	}()

	_, err := container.Run(ctx, integrationRedis,
		container.WithName(name),
		container.WithPullPolicy(container.PullNever),
		container.WithEntrypoint("/does-not-exist-audit"),
		container.WithMounts(container.Mount{
			Type:   container.MountVolume,
			Source: named,
			Target: "/named",
		}),
	)
	if err == nil {
		t.Fatal("want error for bad entrypoint")
	}
	if out, inspectErr := exec.Command("docker", "inspect", name).CombinedOutput(); inspectErr == nil {
		t.Fatalf("container still present after failed Run: %s", out)
	}
	waitForDockerVolume(t, named, true)
}

// TestIntegrationDockerRunFailurePreservesConflict covers #48: a name
// conflict must not delete the pre-existing container.
func TestIntegrationDockerRunFailurePreservesConflict(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	name := fmt.Sprintf("containergo-failkeep-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()

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
	_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
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
	// Stale handle must refuse; replacement must survive.
	if err := oldCtr.Terminate(ctx); err == nil {
		t.Fatal("want error when stale handle deletes replacement")
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
