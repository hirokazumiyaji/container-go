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
// and routes this test to the Docker backend.
func requireDocker(t *testing.T) {
	t.Helper()
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

	ctr, err := container.Run(ctx, "redis:7-alpine",
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
			ctr, err := container.Run(ctx, "alpine:latest",
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
		ctx := context.Background()
		ctr, err := container.Run(ctx, "alpine:latest",
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
