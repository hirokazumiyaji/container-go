//go:build integration

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

const appleLogsCompatCLIEnv = "CONTAINERGO_APPLE_CLI"

// requireAppleLogsCompatCLI returns a version-gated Apple CLI. Set
// CONTAINERGO_APPLE_CLI to a 1.2.2 or 1.3.0 executable and run this test
// once per binary; the repository does not bundle either CLI release.
func requireAppleLogsCompatCLI(t *testing.T) (string, string) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "apple" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Apple logs compatibility checks", backend)
	}

	configured := os.Getenv(appleLogsCompatCLIEnv)
	binary := configured
	if binary == "" {
		binary = "container"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		if configured == "" {
			t.Skip("container CLI not installed")
		}
		t.Fatalf("%s=%q: %v", appleLogsCompatCLIEnv, configured, err)
	}

	out, err := runAppleCompatCommand(t, resolved, "--version")
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", resolved, err, out)
	}
	fields := strings.Fields(string(out))
	version := ""
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "version" {
			version = strings.Trim(fields[i+1], "()")
			break
		}
	}
	switch version {
	case "1.2.2", "1.3.0":
		t.Logf("checking Apple Container CLI %s at %s", version, resolved)
	default:
		t.Skipf("Apple Container CLI %s is outside the #82 compatibility matrix (1.2.2, 1.3.0)", version)
	}
	return resolved, version
}

func runAppleCompatCommand(t *testing.T, binary string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, binary, args...).CombinedOutput()
}

// TestIntegrationAppleLogsCLICompatibility checks the real CLI help and
// argument parser. The -n probe only needs to reach argument validation;
// a missing container or an unavailable system service is expected.
func TestIntegrationAppleLogsCLICompatibility(t *testing.T) {
	binary, version := requireAppleLogsCompatCLI(t)
	t.Run(version, func(t *testing.T) {
		help, err := runAppleCompatCommand(t, binary, "logs", "--help")
		if err != nil {
			t.Fatalf("logs --help: %v\n%s", err, help)
		}
		helpText := string(help)
		if !strings.Contains(helpText, "-n <n>") {
			t.Fatalf("logs help does not advertise -n <n>:\n%s", help)
		}
		for _, unsupported := range []string{"--tail", "--since"} {
			if strings.Contains(helpText, unsupported) {
				t.Errorf("logs help unexpectedly advertises %s:\n%s", unsupported, help)
			}
		}

		missing := fmt.Sprintf("containergo-no-such-logs-compat-%d", time.Now().UnixNano())
		tailOutput, err := runAppleCompatCommand(t, binary, "logs", "-n", "1", missing)
		if err == nil {
			t.Fatalf("logs -n unexpectedly succeeded for missing container %q:\n%s", missing, tailOutput)
		}
		lowerTailOutput := strings.ToLower(string(tailOutput))
		if strings.Contains(lowerTailOutput, "unknown option") || strings.Contains(lowerTailOutput, "usage:") {
			t.Fatalf("logs rejected supported -n:\n%s", tailOutput)
		}

		for _, tc := range []struct {
			flag  string
			value string
		}{
			{flag: "--tail", value: "1"},
			{flag: "--since", value: "2026-01-01T00:00:00Z"},
		} {
			t.Run(tc.flag, func(t *testing.T) {
				out, err := runAppleCompatCommand(t, binary, "logs", tc.flag, tc.value, missing)
				if err == nil {
					t.Fatalf("logs %s unexpectedly succeeded:\n%s", tc.flag, out)
				}
				lower := strings.ToLower(string(out))
				if !strings.Contains(lower, "unknown option") || !strings.Contains(lower, tc.flag) {
					t.Fatalf("logs %s was not rejected as an unknown option:\n%s", tc.flag, out)
				}
			})
		}
	})
}

// TestIntegrationAppleLogsOptionsLive exercises LogsWithOptions against a
// real Apple backend. Run it with CONTAINERGO_APPLE_LIVE=1; optionally set
// CONTAINERGO_APPLE_CLI to the versioned binary to test. The parser-only
// compatibility test above remains part of ordinary integration runs.
func TestIntegrationAppleLogsOptionsLive(t *testing.T) {
	if !appleLogsLiveEnabled() {
		t.Skipf("set %s=1 to run the service-dependent Apple compatibility test", appleLogsLiveEnv)
	}
	binary, version := requireAppleLogsCompatCLI(t)
	if out, err := runAppleCompatCommand(t, binary, "system", "status"); err != nil {
		t.Skipf("Apple Container CLI %s is installed, but its system service is unavailable (%v): %s", version, err, strings.TrimSpace(string(out)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r := &cli.ExecRunner{Binary: binary}
	name := fmt.Sprintf("containergo-logs-compat-%d", time.Now().UnixNano())
	if _, _, err := r.Run(ctx, "run", "--detach", "--name", name, integrationRedis); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _, _ = r.Run(cleanupCtx, "delete", "--force", name)
	})

	ctr := &Container{id: name, runner: r, eng: appleEngine{}}
	rc, err := ctr.LogsWithOptions(ctx, LogsOptions{Tail: 1})
	if err != nil {
		t.Fatalf("LogsWithOptions(Tail): %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close logs: %v", err)
	}

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := ctr.LogsWithOptions(ctx, LogsOptions{Since: since}); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("LogsWithOptions(Since) error = %v, want ErrUnsupportedCapability", err)
	}
	_, stderr, err := r.Run(ctx, "logs", "--since", since.Format(time.RFC3339), name)
	if err == nil {
		t.Fatal("raw Apple logs unexpectedly accepted --since")
	}
	if lower := strings.ToLower(string(stderr)); !strings.Contains(lower, "unknown option '--since'") {
		t.Fatalf("raw Apple logs did not reject --since: %v", err)
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
