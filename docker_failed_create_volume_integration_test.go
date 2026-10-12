//go:build integration

package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func requireDockerRoot(t *testing.T) {
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

type failedCreateVolumeRunner struct {
	base      cli.Runner
	name      string
	volume    string
	runCalled bool
}

func (r *failedCreateVolumeRunner) captureVolume() {
	if r.volume != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _, err := r.base.Run(ctx, "inspect", "--format",
		`{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}`, r.name)
	if err == nil {
		r.volume = strings.TrimSpace(string(out))
	}
}

func (r *failedCreateVolumeRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		r.runCalled = true
		stdout, stderr, err := r.base.Run(ctx, args...)
		if err != nil {
			// The failed create can leave the image-defined anonymous
			// volume behind until cleanupFailedCreate removes it.
			r.captureVolume()
		}
		return stdout, stderr, err
	}
	if args[0] == "rm" || args[0] == "delete" {
		// Capture immediately before rm as a fallback for Docker versions
		// that report the run error before the container is inspectable.
		r.captureVolume()
	}
	return r.base.Run(ctx, args...)
}

func waitForDockerVolumeRoot(t *testing.T, name string, wantExists bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "volume", "inspect", name).CombinedOutput()
		if err == nil {
			if wantExists {
				return
			}
		} else if !wantExists && strings.Contains(strings.ToLower(string(out)), "no such volume") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	out, err := exec.Command("docker", "volume", "inspect", name).CombinedOutput()
	if err == nil {
		t.Fatalf("volume %s still exists, want exists=%t: %s", name, wantExists, out)
	}
	t.Fatalf("volume %s does not exist, want exists=%t: %v: %s", name, wantExists, err, out)
}

func TestIntegrationDockerFailedCreateRemovesAnonymousVolume(t *testing.T) {
	requireDockerRoot(t)
	name := fmt.Sprintf("containergo-failed-volume-%d-%d", os.Getpid(), time.Now().UnixNano())
	runner := &failedCreateVolumeRunner{base: &cli.ExecRunner{Binary: "docker"}, name: name}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
	})

	_, err := Run(context.Background(), "public.ecr.aws/docker/library/redis:7-alpine",
		WithName(name), WithEntrypoint("containergo-missing-entrypoint"), WithCmd("ignored"),
		withRunner(runner), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("Run succeeded, want a create failure")
	}
	if !runner.runCalled {
		t.Skip("image preparation failed before Docker run was attempted")
	}
	if runner.volume == "" {
		t.Fatal("failed Docker run did not expose an image-defined /data volume")
	}
	waitForDockerVolumeRoot(t, runner.volume, false)
}
