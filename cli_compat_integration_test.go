//go:build integration

package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

// requireAppleCapabilityVersion keeps the live matrix tied to the two Apple
// releases whose source/help was used to derive the Go validator. A newer
// CLI is not silently treated as proof of an older contract.
func requireAppleCapabilityVersion(t *testing.T, r *cli.ExecRunner) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, _, err := r.Run(ctx, "system", "version", "--format", "json")
	if err != nil {
		t.Skipf("cannot read Apple Container version: %v", err)
	}
	var entries []struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(stdout, &entries); err != nil {
		t.Skipf("cannot decode Apple Container version %q: %v", stdout, err)
	}
	version := ""
	for _, entry := range entries {
		if entry.AppName == "container" {
			version = entry.Version
			break
		}
	}
	if version == "" && len(entries) > 0 {
		version = entries[0].Version
	}
	if version != "1.2.2" && version != "1.3.0" {
		t.Skipf("Apple Container %s is outside the 1.2.2/1.3.0 capability fixtures", version)
	}
	return version
}

func runAppleCapabilityCase(t *testing.T, r *cli.ExecRunner, args []string, want string) {
	t.Helper()
	name := ""
	for i, arg := range args {
		if arg == "--name" && i+1 < len(args) {
			name = args[i+1]
			break
		}
	}
	if name != "" {
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			_, _, _ = r.Run(cleanupCtx, "delete", "--force", name)
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, err := r.Run(ctx, args...)
	if err == nil {
		t.Fatalf("Apple capability case unexpectedly succeeded: %s (stdout=%s)", strings.Join(args, " "), stdout)
	}
	if want != "" && !strings.Contains(strings.ToLower(string(stdout)+string(stderr)), strings.ToLower(want)) {
		t.Fatalf("Apple capability case %q: stderr %q does not contain %q", strings.Join(args, " "), stderr, want)
	}
}

// TestIntegrationAppleCapabilityMatrix exercises the limits that are easy to
// regress in the Apple CLI itself. It is intentionally version-gated to the
// 1.2.2 and 1.3.0 source/help fixtures; the Go-side early rejection is covered
// by unit tests and does not require a live daemon.
func TestIntegrationAppleCapabilityMatrix(t *testing.T) {
	requireAppleCLI(t)
	eng := appleEngine{}
	r := &cli.ExecRunner{Binary: eng.binary()}
	version := requireAppleCapabilityVersion(t, r)
	t.Logf("checking Apple Container capability matrix for %s", version)

	pullCtx, pullCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	_, pullStderr, pullErr := r.Run(pullCtx, "image", "pull", integrationAlpine)
	pullCancel()
	if pullErr != nil {
		t.Skipf("cannot prepare Apple capability fixture image: %v (%s)", pullErr, pullStderr)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "one-character container name",
			args: []string{"run", "--detach", "--name", "a", integrationAlpine, "true"},
			want: "valid container ID",
		},
		{
			name: "non-Linux platform",
			args: []string{"run", "--detach", "--name", "containergo-cap-platform", "--platform", "windows/amd64", integrationAlpine, "true"},
		},
		{
			name: "invalid network name",
			args: []string{"run", "--detach", "--name", "containergo-cap-network", "--network", "INVALID", integrationAlpine, "true"},
			want: "network",
		},
		{
			name: "host port one",
			args: []string{"run", "--detach", "--name", "containergo-cap-port", "--publish", "1:80", integrationAlpine, "true"},
			want: "invalid publish host port",
		},
		{
			name: "memory below minimum",
			args: []string{"run", "--detach", "--name", "containergo-cap-memory", "--memory", "1K", integrationAlpine, "true"},
			want: "minimum memory amount allowed is 200 MiB",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runAppleCapabilityCase(t, r, tc.args, tc.want)
		})
	}

	// The upstream limit is a count of publish descriptors, not a count of
	// ports in one range. Use unique descriptors to reach exactly 65.
	args := []string{"run", "--detach", "--name", "containergo-cap-count"}
	for i := 0; i < applePublishedPortLimit+1; i++ {
		args = append(args, "--publish", fmt.Sprintf("%d:80/tcp", 20000+i))
	}
	args = append(args, integrationAlpine, "true")
	runAppleCapabilityCase(t, r, args, "cannot exceed more than 64 port publish descriptors")
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
