//go:build integration

package container

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/integrationtest"
)

func requireAppleCLI(t *testing.T) {
	t.Helper()
	integrationtest.Preflight(t, "apple", integrationtest.AppleUnavailable)
}

func requireDockerCLI(t *testing.T) {
	t.Helper()
	integrationtest.Preflight(t, "docker", integrationtest.DockerUnavailable)
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
	if !eng.nameConflict(conflictErr) {
		t.Fatalf("nameConflict = false for %v", conflictErr)
	}

	_, _, missingImg := r.Run(ctx, "image", "inspect", "no-such-image-containergo:never")
	if missingImg == nil {
		t.Fatal("want missing image error")
	}
	if !eng.imageMissing(missingImg) {
		t.Fatalf("imageMissing = false for %v", missingImg)
	}

	_, _, missingCtr := r.Run(ctx, "inspect", "no-such-ctr-containergo")
	if missingCtr == nil {
		t.Fatal("want missing container error")
	}
	if !eng.containerMissing(missingCtr) {
		t.Fatalf("containerMissing = false for %v", missingCtr)
	}
	if !isNotFound(missingCtr) {
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
		_, _, _ = r.Run(context.Background(), "rm", "--force", "--volumes", name)
	})

	_, _, conflictErr := r.Run(ctx, "run", "-d", "--name", name, integrationRedis)
	if conflictErr == nil {
		t.Fatal("want name conflict error")
	}
	if !eng.nameConflict(conflictErr) {
		t.Fatalf("nameConflict = false for %v", conflictErr)
	}

	_, _, missingImg := r.Run(ctx, "image", "inspect", "no-such-image-containergo:never")
	if missingImg == nil {
		t.Fatal("want missing image error")
	}
	if !eng.imageMissing(missingImg) {
		t.Fatalf("imageMissing = false for %v", missingImg)
	}

	_, _, missingCtr := r.Run(ctx, "inspect", "no-such-ctr-containergo")
	if missingCtr == nil {
		t.Fatal("want missing container error")
	}
	if !eng.containerMissing(missingCtr) {
		t.Fatalf("containerMissing = false for %v", missingCtr)
	}
	if !isNotFound(missingCtr) {
		t.Fatalf("isNotFound = false for %v", missingCtr)
	}
}
