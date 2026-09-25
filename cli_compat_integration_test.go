//go:build integration

package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func requireAppleCLI(t *testing.T) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "apple" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Apple CLI checks", backend)
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container CLI not installed")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skip("apple container system service not running; run `container system start`")
	}
}

func requireDockerCLI(t *testing.T) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "docker" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Docker CLI checks", backend)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
}

// TestIntegrationAppleCLIErrorMatchers exercises nameConflict,
// imageMissing, and containerMissing against live Apple Container stderr.
func TestIntegrationAppleCLIErrorMatchers(t *testing.T) {
	requireAppleCLI(t)
	eng := appleEngine{}
	r := &cli.ExecRunner{Binary: eng.binary()}
	ctx := context.Background()
	name := fmt.Sprintf("containergo-compat-%d", time.Now().UnixNano())

	if _, _, err := r.Run(ctx, "run", "--detach", "--name", name, integrationRedis); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = r.Run(context.Background(), "delete", "--force", name)
	})

	_, _, conflictErr := r.Run(ctx, "run", "--detach", "--name", name, integrationRedis)
	if conflictErr == nil {
		t.Fatal("want name conflict error")
	}
	if !eng.nameConflict(lifecycleRun, name, conflictErr) {
		t.Fatalf("nameConflict = false for %v", conflictErr)
	}

	_, _, missingImg := r.Run(ctx, "image", "inspect", "no-such-image-containergo:never")
	if missingImg == nil {
		t.Fatal("want missing image error")
	}
	if !eng.imageMissing(missingImg) {
		t.Fatalf("imageMissing = false for %v", missingImg)
	}

	missingName := "no-such-ctr-containergo"
	_, _, missingCtr := r.Run(ctx, "inspect", missingName)
	if missingCtr == nil {
		t.Fatal("want missing container error")
	}
	if !eng.containerMissing(lifecycleInspect, missingName, missingCtr) {
		t.Fatalf("containerMissing = false for %v", missingCtr)
	}
	if !isContainerNotFound(eng, lifecycleInspect, missingName, missingCtr) {
		t.Fatalf("isNotFound = false for %v", missingCtr)
	}
}

// TestIntegrationDockerCLIErrorMatchers exercises the Docker matchers
// against a live daemon.
func TestIntegrationDockerCLIErrorMatchers(t *testing.T) {
	requireDockerCLI(t)
	eng := dockerEngine{}
	r := &cli.ExecRunner{Binary: eng.binary()}
	ctx := context.Background()
	name := fmt.Sprintf("containergo-compat-%d", time.Now().UnixNano())

	if _, _, err := r.Run(ctx, "run", "-d", "--name", name, integrationRedis); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = r.Run(context.Background(), "rm", "--force", name)
	})

	_, _, conflictErr := r.Run(ctx, "run", "-d", "--name", name, integrationRedis)
	if conflictErr == nil {
		t.Fatal("want name conflict error")
	}
	if !eng.nameConflict(lifecycleRun, name, conflictErr) {
		t.Fatalf("nameConflict = false for %v", conflictErr)
	}

	_, _, missingImg := r.Run(ctx, "image", "inspect", "no-such-image-containergo:never")
	if missingImg == nil {
		t.Fatal("want missing image error")
	}
	if !eng.imageMissing(missingImg) {
		t.Fatalf("imageMissing = false for %v", missingImg)
	}

	missingName := "no-such-ctr-containergo"
	_, _, missingCtr := r.Run(ctx, "inspect", missingName)
	if missingCtr == nil {
		t.Fatal("want missing container error")
	}
	if !eng.containerMissing(lifecycleInspect, missingName, missingCtr) {
		t.Fatalf("containerMissing = false for %v", missingCtr)
	}
	if !isContainerNotFound(eng, lifecycleInspect, missingName, missingCtr) {
		t.Fatalf("isNotFound = false for %v", missingCtr)
	}
}
