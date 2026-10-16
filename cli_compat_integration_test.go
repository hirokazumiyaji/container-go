//go:build integration

package container

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
	"github.com/hirokazumiyaji/container-go/internal/integrationtest"
)

// appleCapabilityLiveEnv opts into the Apple capability matrix, which creates
// real containers and is never run by default.
const appleCapabilityLiveEnv = "CONTAINERGO_APPLE_LIVE"

func requireAppleCLI(t *testing.T) {
	t.Helper()
	integrationtest.Preflight(t, "apple", integrationtest.AppleUnavailable)
}

func requireDockerCLI(t *testing.T) {
	t.Helper()
	integrationtest.Preflight(t, "docker", integrationtest.DockerUnavailable)
}

func dockerPullRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "toomanyrequests") || strings.Contains(msg, "Rate exceeded")
}

func runDockerSeedWithPullRetry(t *testing.T, r *cli.ExecRunner, ctx context.Context, args ...string) error {
	t.Helper()
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, _, err = r.Run(ctx, args...)
		if err == nil {
			return nil
		}
		if !dockerPullRateLimited(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	if dockerPullRateLimited(err) {
		t.Skipf("docker pull rate-limited after retries: %v", err)
	}
	return err
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

func requireAppleCapabilityLive(t *testing.T) {
	t.Helper()
	if os.Getenv(appleCapabilityLiveEnv) != "1" {
		t.Skipf("set %s=1 to run the Apple capability matrix", appleCapabilityLiveEnv)
	}
	// TestMain clears CONTAINERGO_BACKEND, so the selection must come
	// from the recorded value.
	if backend := integrationtest.SelectedBackend(); backend != "apple" {
		t.Skipf("the Apple capability matrix requires CONTAINERGO_BACKEND=apple (got %q)", backend)
	}
	requireAppleCLI(t)
}

// TestIntegrationAppleCLIErrorMatchers exercises nameConflict,
// imageMissing, and containerMissing against live Apple Container stderr.
func TestIntegrationAppleCLIErrorMatchers(t *testing.T) {
	requireAppleCLI(t)
	eng := appleEngine{}
	r := &cli.ExecRunner{Binary: eng.binary()}
	ctx := context.Background()
	token := newAppleCapabilityToken(t)
	name := appleCapabilityName(token, "compat")

	if _, _, err := r.Run(ctx, "run", "--detach", "--name", name, "--label", appleCapabilityLabel+"="+token, integrationRedis); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		cleanupAppleCapabilityContainer(t, r, name, token)
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

	missingName := "no-such-ctr-containergo"
	_, _, missingCtr := r.Run(ctx, "inspect", missingName)
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

const appleCapabilityLabel = "com.github.hirokazumiyaji.container-go.capability-test"

var appleCapabilityFixtureVersions = map[string]struct{}{
	"1.2.2": {},
	"1.3.0": {},
}

type appleVersionEntry struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// requireAppleCapabilityVersion keeps the live matrix tied to the Apple
// releases whose source/help was used to derive the Go validator. It also
// records the API-server version separately: a CLI-only response is not
// evidence that the server/API version is known.
func requireAppleCapabilityVersion(t *testing.T, r *cli.ExecRunner) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr, err := r.Run(ctx, "system", "version", "--format", "json")
	if err != nil {
		t.Skipf("cannot read Apple Container version: %v (%s)", err, stderr)
	}
	var entries []appleVersionEntry
	if err := json.Unmarshal(stdout, &entries); err != nil {
		t.Skipf("cannot decode Apple Container version %q: %v", stdout, err)
	}

	cliVersion := ""
	apiVersion := ""
	for _, entry := range entries {
		switch entry.AppName {
		case "container":
			if cliVersion == "" {
				cliVersion = normalizeAppleVersion(entry.Version)
			}
		case "container-apiserver":
			if apiVersion == "" {
				apiVersion = normalizeAppleVersion(entry.Version)
			}
		}
	}
	if cliVersion == "" {
		t.Skipf("Apple Container version response has no container CLI entry: %q", stdout)
	}
	if _, ok := appleCapabilityFixtureVersions[cliVersion]; !ok {
		t.Skipf("Apple Container CLI %s is outside the 1.2.2/1.3.0 capability fixtures", cliVersion)
	}

	// Older service combinations can omit the server component from
	// `system version`; status JSON exposes the API-server version in either
	// the legacy top-level or nested server.version shape.
	if apiVersion == "" {
		statusCtx, statusCancel := context.WithTimeout(context.Background(), 30*time.Second)
		statusOut, statusErr, statusRunErr := r.Run(statusCtx, "system", "status", "--format", "json")
		statusCancel()
		if statusRunErr != nil {
			t.Skipf("Apple Container API server version is unavailable from system status: %v (%s)", statusRunErr, statusErr)
		}
		decodedVersion, decodeErr := decodeAppleStatusVersion(statusOut)
		if decodeErr != nil {
			t.Skipf("cannot decode Apple Container system status %q: %v", statusOut, decodeErr)
		}
		apiVersion = decodedVersion
	}
	if apiVersion == "" {
		t.Skipf("Apple Container CLI %s is available, but the API server version is unavailable; skipping the live matrix", cliVersion)
	}
	if _, ok := appleCapabilityFixtureVersions[apiVersion]; !ok {
		t.Skipf("Apple Container API server %s is outside the 1.2.2/1.3.0 capability fixtures", apiVersion)
	}
	if apiVersion != cliVersion {
		t.Skipf("Apple Container CLI %s and API server %s differ; skipping the live matrix", cliVersion, apiVersion)
	}
	t.Logf("using Apple Container CLI/API server version %s", cliVersion)
	return cliVersion
}

func newAppleCapabilityToken(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate Apple capability test identity: %v", err)
	}
	return fmt.Sprintf("%d-%x", os.Getpid(), b)
}

func appleCapabilityName(token, suffix string) string {
	return "containergo-cap-" + token + "-" + suffix
}

func appleCapabilityArgs(name, token string, extra ...string) []string {
	args := []string{"run", "--detach", "--name", name, "--label", appleCapabilityLabel + "=" + token}
	args = append(args, extra...)
	return append(args, integrationAlpine, "true")
}

func requireAppleCapabilityNameFree(t *testing.T, r *cli.ExecRunner, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, stderr, err := r.Run(ctx, "inspect", name)
	if err == nil {
		t.Skipf("refusing to touch existing Apple container %q", name)
	}
	if !isNotFound(err) {
		t.Skipf("cannot verify that Apple container %q is unused; refusing cleanup: %v (%s)", name, err, stderr)
	}
}

func appleCapabilityOwned(r *cli.ExecRunner, name, token string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, _, err := r.Run(ctx, "inspect", name)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	containers, err := inspect.Decode(stdout)
	if err != nil {
		return false, err
	}
	for _, ctr := range containers {
		if ctr.ID == name && ctr.Configuration.Labels[appleCapabilityLabel] == token {
			return true, nil
		}
	}
	return false, nil
}

func cleanupAppleCapabilityContainer(t *testing.T, r *cli.ExecRunner, name, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	unlock, err := lockName(ctx, name)
	if err != nil {
		t.Logf("Apple capability cleanup %s: lock name: %v", name, err)
		return
	}
	defer unlock()

	owned, err := appleCapabilityOwned(r, name, token)
	if err != nil {
		t.Logf("Apple capability cleanup %s: inspect ownership: %v", name, err)
		return
	}
	if !owned {
		// A name collision or an external replacement must never turn into
		// deletion of a container that this test did not create.
		return
	}
	_, stderr, err := r.Run(ctx, "delete", "--force", name)
	if err != nil && !isNotFound(err) {
		t.Logf("Apple capability cleanup %s: delete: %v (%s)", name, err, stderr)
	}
}

func dockerLiveOwned(r *cli.ExecRunner, name, token string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, _, err := r.Run(ctx, "inspect", name)
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	info, err := (dockerEngine{}).parseInspect(stdout, name)
	if err != nil {
		return "", false, err
	}
	if info.labels[appleCapabilityLabel] != token {
		return "", false, nil
	}
	if !dockerIDRE.MatchString(info.uid) {
		return "", false, fmt.Errorf("Docker inspect returned invalid immutable container ID %q", info.uid)
	}
	return info.uid, true, nil
}

func cleanupDockerLiveContainer(t *testing.T, r *cli.ExecRunner, name, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	unlock, err := lockName(ctx, name)
	if err != nil {
		t.Logf("Docker live-test cleanup %s: lock name: %v", name, err)
		return
	}
	defer unlock()

	uid, owned, err := dockerLiveOwned(r, name, token)
	if err != nil {
		t.Logf("Docker live-test cleanup %s: inspect ownership: %v", name, err)
		return
	}
	if !owned {
		return
	}
	_, stderr, err := r.Run(ctx, "rm", "--force", uid)
	if err != nil && !isNotFound(err) {
		t.Logf("Docker live-test cleanup %s: delete: %v (%s)", name, err, stderr)
	}
}

func TestIntegrationDockerLiveCleanupUsesImmutableID(t *testing.T) {
	// Keep the other integration tests available on Windows; only this fake
	// executable depends on a POSIX shell.
	if runtime.GOOS == "windows" {
		t.Skip("the fake Docker CLI requires a POSIX shell")
	}

	dir := t.TempDir()
	logPath := dir + "/calls.log"
	uid := strings.Repeat("ab", 32)
	token := "cleanup-token"
	data := fmt.Sprintf(
		`[{"Id":%q,"Name":"/race-name","State":{"Status":"running"},"Config":{"Labels":{%q:%q}}}]`,
		uid, appleCapabilityLabel, token,
	)
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  cat <<'JSON'\n" + data + "\nJSON\n" +
		"fi\n"
	binPath := dir + "/docker"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupDockerLiveContainer(t, &cli.ExecRunner{Binary: binPath}, "race-name", token)
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(got)
	if !strings.Contains(calls, "rm --force "+uid) {
		t.Fatalf("cleanup calls = %q, want rm by immutable ID %s", calls, uid)
	}
	if strings.Contains(calls, "rm --force race-name") {
		t.Fatalf("cleanup deleted mutable name after inspecting: %q", calls)
	}
}

func runAppleCapabilityCase(t *testing.T, r *cli.ExecRunner, name, token string, args []string, want string) {
	t.Helper()
	if want == "" {
		t.Fatal("Apple capability case has no assertion")
	}
	requireAppleCapabilityNameFree(t, r, name)
	defer cleanupAppleCapabilityContainer(t, r, name, token)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, err := r.Run(ctx, args...)
	if err == nil {
		t.Fatalf("Apple capability case unexpectedly succeeded: %s (stdout=%s)", strings.Join(args, " "), stdout)
	}
	output := strings.ToLower(string(stdout) + "\n" + string(stderr))
	if !strings.Contains(output, strings.ToLower(want)) {
		t.Fatalf("Apple capability case %q: output %q does not contain %q", strings.Join(args, " "), output, want)
	}
}

// TestIntegrationAppleCapabilityMatrix is an explicitly opt-in live test. It
// is version-gated to the 1.2.2 and 1.3.0 fixtures, uses one ownership label
// and unique name per case, and never deletes a name whose label is not its
// own. The Go-side early rejection is covered by unit tests.
func TestIntegrationAppleCapabilityMatrix(t *testing.T) {
	requireAppleCapabilityLive(t)
	eng := appleEngine{}
	r := &cli.ExecRunner{Binary: eng.binary()}
	version := requireAppleCapabilityVersion(t, r)
	t.Logf("checking Apple Container capability matrix for CLI/API %s", version)

	pullCtx, pullCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	_, pullStderr, pullErr := r.Run(pullCtx, "image", "pull", integrationAlpine)
	pullCancel()
	if pullErr != nil {
		t.Skipf("cannot prepare Apple capability fixture image: %v (%s)", pullErr, pullStderr)
	}

	token := newAppleCapabilityToken(t)
	cases := []struct {
		name   string
		suffix string
		extra  []string
		want   string
	}{
		{
			name:   "one-character container name",
			suffix: "name",
			want:   "valid container ID",
		},
		{
			name:   "bare Linux platform",
			suffix: "bare-platform",
			extra:  []string{"--platform", "linux"},
			want:   "missing architecture",
		},
		{
			name:   "non-Linux platform",
			suffix: "platform",
			extra:  []string{"--platform", "windows/amd64"},
			// Apple renders the error code and message separately, for
			// example: unsupported: "platform windows/amd64".
			want: "unsupported",
		},
		{
			name:   "invalid platform variant",
			suffix: "variant",
			extra:  []string{"--platform", "linux/arm64/v7"},
			want:   "invalid variant",
		},
		{
			name:   "invalid network name",
			suffix: "network",
			extra:  []string{"--network", "INVALID"},
			want:   "network",
		},
		{
			name:   "host port one",
			suffix: "port",
			extra:  []string{"--publish", "1:80"},
			want:   "invalid publish host port",
		},
		{
			name:   "memory below minimum",
			suffix: "memory",
			extra:  []string{"--memory", "1K"},
			want:   "minimum memory amount allowed is 200 MiB",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The one-character case necessarily uses "a"; the preflight
			// refuses to touch it if a user already owns that name.
			name := "a"
			if tc.suffix != "name" {
				name = appleCapabilityName(token, tc.suffix)
			}
			runAppleCapabilityCase(t, r, name, token, appleCapabilityArgs(name, token, tc.extra...), tc.want)
		})
	}

	// The upstream limit is a count of publish descriptors, not a count of
	// ports in one range. Use unique descriptors to reach exactly 65.
	name := appleCapabilityName(token, "count")
	args := []string{"run", "--detach", "--name", name, "--label", appleCapabilityLabel + "=" + token}
	for i := 0; i < applePublishedPortLimit+1; i++ {
		args = append(args, "--publish", fmt.Sprintf("%d:80/tcp", 20000+i))
	}
	args = append(args, integrationAlpine, "true")
	runAppleCapabilityCase(t, r, name, token, args, "cannot exceed more than 64 port publish descriptors")
}

// TestIntegrationDockerCLIErrorMatchers exercises the Docker matchers
// against a live daemon.
func TestIntegrationDockerCLIErrorMatchers(t *testing.T) {
	requireDockerCLI(t)
	eng := dockerEngine{}
	r := &cli.ExecRunner{Binary: eng.binary()}
	ctx := context.Background()
	token := newAppleCapabilityToken(t)
	name := appleCapabilityName(token, "compat")

	if err := runDockerSeedWithPullRetry(t, r, ctx, "run", "-d", "--name", name, "--label", appleCapabilityLabel+"="+token, integrationRedis); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		cleanupDockerLiveContainer(t, r, name, token)
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

	missingName := "no-such-ctr-containergo"
	_, _, missingCtr := r.Run(ctx, "inspect", missingName)
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
