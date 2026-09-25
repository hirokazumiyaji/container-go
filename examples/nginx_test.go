//go:build integration

package examples

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestExampleNginx(t *testing.T) {
	requireSystem(t)
	ctx := context.Background()

	ctr, err := container.Run(ctx, integrationNginx,
		container.WithExposedPorts("80/tcp"),
		container.WithWaitStrategy(wait.ForHTTP("/")),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatal(err)
	}

	// Apple uses the container IP directly; Docker uses the
	// daemon-assigned published endpoint for this exposed port.
	endpoint, err := ctr.Endpoint(ctx, "80/tcp")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + endpoint + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "nginx") {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}
