//go:build integration

// Package examples holds runnable usage examples. They need Apple
// Container running: `container system start`, then
// `go test -tags integration ./examples/`.
package examples

import (
	"context"
	"net"
	"os/exec"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

func requireSystem(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container CLI not installed")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skip("apple container system service not running")
	}
}

func TestExampleRedis(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	ctr, err := container.Run(ctx, "redis:7-alpine",
		container.WithExposedPorts("6379/tcp"),
		container.WithEnv(map[string]string{"REDIS_ARGS": "--appendonly yes"}),
		container.WithWaitStrategy(wait.ForAll(
			wait.ForLog("Ready to accept connections"),
			wait.ForListeningPort("6379/tcp"),
		)),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatal(err)
	}

	endpoint, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}

	// Any redis client would take endpoint here; a raw PING keeps the
	// example dependency-free.
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "+PONG\r\n" {
		t.Fatalf("reply = %q, want +PONG", buf[:n])
	}
}
