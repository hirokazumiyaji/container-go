//go:build integration

package container_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	assertIntegrationCopyOutRejectsSpecialFiles(t, ctx, ctr)

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
		_ = exec.Command("docker", "rm", "--force", name).Run()
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
