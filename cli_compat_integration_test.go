//go:build integration

package container

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

func requireAppleCLI(t *testing.T) {
	t.Helper()
	if backend := os.Getenv("CONTAINERGO_BACKEND"); backend != "" && backend != "apple" {
		t.Skipf("CONTAINERGO_BACKEND=%s; skipping Apple CLI checks", backend)
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container CLI not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "container", "system", "status").Run(); err != nil {
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

func requireAppleCapabilityLive(t *testing.T) {
	t.Helper()
	if os.Getenv(appleCapabilityLiveEnv) != "1" {
		t.Skipf("set %s=1 to run the Apple capability matrix", appleCapabilityLiveEnv)
	}
	if backend := os.Getenv(backendEnv); backend != "apple" {
		t.Skipf("the Apple capability matrix requires %s=apple (got %q)", backendEnv, backend)
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

const appleCapabilityLabel = "com.github.hirokazumiyaji.container-go.capability-test"

var appleCapabilityFixtureVersions = map[string]struct{}{
	"1.2.2": {},
	"1.3.0": {},
}

type appleVersionEntry struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

type appleStatusVersion struct {
	APIServerVersion string `json:"apiServerVersion"`
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
	// `system version`; status JSON exposes the same API-server fields.
	if apiVersion == "" {
		statusCtx, statusCancel := context.WithTimeout(context.Background(), 30*time.Second)
		statusOut, statusErr, statusRunErr := r.Run(statusCtx, "system", "status", "--format", "json")
		statusCancel()
		if statusRunErr != nil {
			t.Skipf("Apple Container API server version is unavailable from system status: %v (%s)", statusRunErr, statusErr)
		}
		var status appleStatusVersion
		if err := json.Unmarshal(statusOut, &status); err != nil {
			t.Skipf("cannot decode Apple Container system status %q: %v", statusOut, err)
		}
		apiVersion = normalizeAppleVersion(status.APIServerVersion)
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
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	uid := strings.Repeat("ab", 32)
	token := "cleanup-token"
	data := fmt.Sprintf(`[{"Id":%q,"Name":"/race-name","Config":{"Labels":{%q:%q}}}]`, uid, appleCapabilityLabel, token)
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

	if _, _, err := r.Run(ctx, "run", "-d", "--name", name, "--label", appleCapabilityLabel+"="+token, integrationRedis); err != nil {
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
