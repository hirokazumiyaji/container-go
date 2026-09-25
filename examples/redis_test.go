//go:build integration

// Package examples holds runnable usage examples. They need a backend:
// Apple Container (`container system start`) or Docker (running daemon),
// then `go test -tags integration ./examples/`.
package examples

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestMain(m *testing.M) {
	switch backend := os.Getenv("CONTAINERGO_BACKEND"); backend {
	case "", "apple", "docker":
	default:
		fmt.Fprintf(os.Stderr, "invalid CONTAINERGO_BACKEND=%q: valid values are \"apple\" and \"docker\"\n", backend)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func requireSystem(t *testing.T) {
	t.Helper()
	backend := os.Getenv("CONTAINERGO_BACKEND")
	if backend == "" {
		// Match detectEngineFor: darwin → Apple Container, else Docker.
		if runtime.GOOS == "darwin" {
			backend = "apple"
		} else {
			backend = "docker"
		}
	}
	switch backend {
	case "apple":
		if _, err := exec.LookPath("container"); err != nil {
			t.Skip("container CLI not installed")
		}
		if err := exec.Command("container", "system", "status").Run(); err != nil {
			t.Skip("apple container system service not running")
		}
	case "docker":
		if _, err := exec.LookPath("docker"); err != nil {
			t.Skip("docker CLI not installed")
		}
		if err := exec.Command("docker", "info").Run(); err != nil {
			t.Skip("docker daemon not running")
		}
	default:
		t.Fatalf("invalid CONTAINERGO_BACKEND=%q: valid values are \"apple\" and \"docker\"", backend)
	}
}

func TestExampleRedis(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	ctr, err := container.Run(ctx, integrationRedis,
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
