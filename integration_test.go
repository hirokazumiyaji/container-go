//go:build integration

package container_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

// requireSystem skips unless the Apple Container CLI is installed and
// its system service answers. When CONTAINERGO_BACKEND is set to a
// non-apple value, Apple integration tests are skipped.
func requireSystem(t *testing.T) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "apple" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Apple integration", backend)
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container CLI not installed")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skip("apple container system service not running; run `container system start`")
	}
}

func TestIntegrationRedisLifecycle(t *testing.T) {
	requireSystem(t)
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

	// Direct-IP connectivity without any published port.
	endpoint, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	conn.Close()

	// Exec inside the container.
	code, out, err := ctr.Exec(ctx, []string{"redis-cli", "ping"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, _ := io.ReadAll(out)
	if code != 0 || !strings.Contains(string(data), "PONG") {
		t.Errorf("redis-cli ping: code=%d out=%q", code, data)
	}

	// Copy a file in and read it back.
	src := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(src, []byte("hello from host"), 0o600); err != nil {
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
	if string(round) != "hello from host" {
		t.Errorf("round-tripped content = %q", round)
	}

	// Logs snapshot.
	logs, err := ctr.Logs(ctx)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer logs.Close()
	logData, _ := io.ReadAll(logs)
	if !strings.Contains(string(logData), "Ready to accept connections") {
		t.Errorf("logs missing readiness line: %q", logData)
	}

	// Terminate removes the container.
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := ctr.State(ctx); err == nil {
		t.Error("State after Terminate: want error, got nil")
	}
}

func TestIntegrationPublishedPort(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	ctr, err := container.Run(ctx, integrationNginx,
		container.WithExposedPorts("80/tcp"),
		container.WithPublishedPort("127.0.0.1:18080:80"),
		container.WithWaitStrategy(wait.ForHTTP("/")),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	host, _ := ctr.Host(ctx)
	port, _ := ctr.MappedPort(ctx, "80/tcp")
	if host != "127.0.0.1" || port != 18080 {
		t.Fatalf("endpoint = %s:%d, want 127.0.0.1:18080", host, port)
	}
	resp, err := http.Get(fmt.Sprintf("http://%s:%d/", host, port))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestIntegrationParallelStarts(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()
	const n = 10

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
				return
			}
			if _, err := ctr.ContainerIP(ctx); err != nil {
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

// TestIntegrationLazyInspectStateAndWaitRollback covers #20: Run without
// a wait strategy still reports State on demand, and a connection wait
// that never succeeds removes the container.
func TestIntegrationLazyInspectStateAndWaitRollback(t *testing.T) {
	requireSystem(t)
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
	if out, inspectErr := exec.Command("container", "inspect", name).CombinedOutput(); inspectErr == nil {
		t.Fatalf("container still present after wait rollback: %s", out)
	}
}

func TestIntegrationReuseSharedAcrossProcesses(t *testing.T) {
	if os.Getenv("CONTAINERGO_REUSE_CHILD") == "1" {
		requireSystem(t)
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

	requireSystem(t)
	name := fmt.Sprintf("containergo-reuse-%d", os.Getpid())
	defer func() {
		_, _ = container.PruneReuseGroup(context.Background(), "integration-reuse")
		_ = exec.Command("container", "delete", "--force", name).Run()
	}()

	startChild := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestIntegrationReuseSharedAcrossProcesses$")
		cmd.Env = append(os.Environ(),
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
}
