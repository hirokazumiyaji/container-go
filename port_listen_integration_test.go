//go:build integration

package container_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestIntegrationDockerListeningPortWaitsForDelayedServer(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctr, err := container.Run(ctx, integrationRedis,
		container.WithCmd("sh", "-c", "sleep 2; exec redis-server --save '' --appendonly no"),
		container.WithExposedPorts("6379/tcp"),
		container.WithWaitStrategy(wait.ForListeningPort("6379/tcp").WithStartupTimeout(20*time.Second)),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	code, output, err := ctr.Exec(ctx, []string{"redis-cli", "ping"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(string(data), "PONG") {
		t.Fatalf("port wait returned before Redis was ready: code=%d output=%q", code, data)
	}
}
