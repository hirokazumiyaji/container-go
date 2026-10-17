package container

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func freshVariant(platform, digest string) map[string]any {
	parts := strings.Split(platform, "/")
	metadata := map[string]any{
		"os":           parts[0],
		"architecture": parts[1],
	}
	if len(parts) == 3 {
		metadata["variant"] = parts[2]
	}
	return map[string]any{
		"platform": metadata,
		"digest":   digest,
	}
}

func freshHostPlatform() string {
	arch := runtime.GOARCH
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	return "linux/" + arch
}

func freshPlatformMap() map[string]string {
	parts := strings.Split(freshHostPlatform(), "/")
	platform := map[string]string{"os": parts[0], "architecture": parts[1]}
	if len(parts) == 3 {
		platform["variant"] = parts[2]
	}
	return platform
}

func freshAppleImageJSON(root, variant string) []byte {
	return platformPinningAppleImageJSONForRoot(root, []map[string]any{
		freshVariant(freshHostPlatform(), variant),
	})
}

func freshOtherPlatform(host string) string {
	platform, ok := parseApplePlatformSelector(host)
	if ok && platform.architecture == "arm64" {
		return "linux/amd64"
	}
	return "linux/arm64/v8"
}

func TestAppleBareArmelPlatformIsCanonicalizedForRun(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "")
	image := platformPinningAppleImageJSON([]map[string]any{
		freshVariant("linux/armel/v6", platformPinningVariant),
	})
	runner := &platformPinningRunner{inspectJSON: image}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullAlways), WithPlatform("linux/armel"),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.platform != "linux/armel/v6" {
		t.Fatalf("identity platform = %q, want linux/armel/v6", ctr.image.platform)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && (call[0] == "run" || (len(call) > 1 && call[0] == "image" && call[1] == "pull")) &&
			!callHasPlatform(call, "linux/armel/v6") {
			t.Errorf("bare armel selector was not canonicalized: %v", call)
		}
	}
}

func TestAppleDefaultArmelPlatformIsCanonicalizedForPull(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/armel")
	image := platformPinningAppleImageJSON([]map[string]any{
		freshVariant("linux/armel/v6", platformPinningVariant),
	})
	runner := &platformPinningRunner{inspectJSON: image}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullAlways),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.platform != "linux/armel/v6" {
		t.Fatalf("identity platform = %q, want canonical default", ctr.image.platform)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && (call[0] == "run" || (len(call) > 1 && call[0] == "image" && call[1] == "pull")) &&
			!callHasPlatform(call, "linux/armel/v6") {
			t.Errorf("default armel selector was not canonicalized: %v", call)
		}
	}
}

func TestPullCanonicalizesDefaultArmelPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/armel")
	runner := &platformPinningRunner{}
	if err := pullWith(context.Background(), runner, appleEngine{}, "redis:7-alpine"); err != nil {
		t.Fatalf("pullWith: %v", err)
	}
	if len(runner.calls) != 1 || !callHasPlatform(runner.calls[0], "linux/armel/v6") {
		t.Fatalf("pull calls = %v, want canonical default armel", runner.calls)
	}
}

func TestPullWithoutSelectorRemainsPlatformAgnostic(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "")
	runner := &platformPinningRunner{}
	if err := pullWith(context.Background(), runner, appleEngine{}, "redis:7-alpine"); err != nil {
		t.Fatalf("pullWith: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("pull calls = %v, want one pull", runner.calls)
	}
	if callHasPlatform(runner.calls[0], "linux/arm64") ||
		callHasPlatform(runner.calls[0], "linux/amd64") ||
		callHasPlatform(runner.calls[0], "linux/armel/v6") {
		t.Fatalf("standalone Pull unexpectedly selected a host platform: %v", runner.calls[0])
	}
	for _, arg := range runner.calls[0] {
		if arg == "--platform" {
			t.Fatalf("standalone Pull unexpectedly passed a platform flag: %v", runner.calls[0])
		}
	}
}

func TestAppleRunMaterializesHostPlatformForPullNever(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "")
	host := freshHostPlatform()
	other := freshOtherPlatform(host)
	image := platformPinningAppleImageJSONForRoot(platformPinningRoot, []map[string]any{
		freshVariant(other, "sha256:"+platformPinningID),
	})
	runner := &platformPinningRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever),
		withRunner(runner), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal for implicit %s", err, other)
	}
	runImage, _, pulls := runner.state()
	if runImage != "" || pulls != 0 {
		t.Fatalf("implicit host mismatch ran or fetched: run=%q pulls=%d", runImage, pulls)
	}
}

func TestAppleRunUsesHostPlatformBeforeFetchingMissingVariant(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "")
	host := freshHostPlatform()
	other := freshOtherPlatform(host)
	initial := platformPinningAppleImageJSONForRoot(platformPinningRoot, []map[string]any{
		freshVariant(other, "sha256:"+platformPinningID),
	})
	afterPull := platformPinningAppleImageJSONForRoot(platformPinningRoot, []map[string]any{
		freshVariant(host, platformPinningVariant),
		freshVariant(other, "sha256:"+platformPinningID),
	})
	runner := &platformPinningRunner{inspectJSON: initial, afterPullJSON: afterPull}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.platform != host {
		t.Fatalf("identity platform = %q, want materialized host %q", ctr.image.platform, host)
	}
	runImage, _, pulls := runner.state()
	if runImage != "redis:7-alpine@"+platformPinningRoot || pulls != 1 {
		t.Fatalf("run=%q pulls=%d, want host-specific fetch", runImage, pulls)
	}
	var sawPull bool
	for _, call := range runner.calls {
		if len(call) > 1 && call[0] == "image" && call[1] == "pull" {
			sawPull = true
			if !callHasPlatform(call, host) {
				t.Errorf("pull missing materialized host platform: %v", call)
			}
		}
		if len(call) > 0 && call[0] == "run" && !callHasPlatform(call, host) {
			t.Errorf("run missing materialized host platform: %v", call)
		}
	}
	if !sawPull {
		t.Fatalf("missing host variant was not fetched: %v", runner.calls)
	}
}

func TestAppleRunRejectsImplicitPostCreatePlatformMismatch(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "")
	host := freshHostPlatform()
	runner := &finalRunner{
		imageJSON:        finalMultiFixture(),
		exactAddressable: true,
		postRoot:         finalRoot,
		postPlatform:     freshOtherPlatform(host),
	}
	_, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want post-create implicit platform mismatch", err)
	}
	if runner.deleteCalls != 1 || runner.runImage == "" {
		t.Fatalf("implicit platform mismatch was not rolled back: run=%q deletes=%d", runner.runImage, runner.deleteCalls)
	}
}

func TestApplePinnedComparisonIgnoresTagOnlyWithDigest(t *testing.T) {
	root := platformPinningRoot
	cases := []struct {
		name      string
		requested string
		actual    string
		want      bool
	}{
		{
			name:      "tagged and canonical tagless",
			requested: "registry.example/team/demo:stable@" + root,
			actual:    "registry.example/team/demo@" + root,
			want:      true,
		},
		{
			name:      "custom registry canonicalization",
			requested: "team/demo:stable@" + root,
			actual:    "registry.example/team/demo@" + root,
			want:      true,
		},
		{
			name:      "different nonsemantic tags",
			requested: "team/demo:stable@" + root,
			actual:    "team/demo:other@" + root,
			want:      true,
		},
		{
			name:      "different digest",
			requested: "team/demo:stable@" + root,
			actual:    "team/demo:stable@sha256:" + platformPinningID,
			want:      false,
		},
		{
			name:      "different repository",
			requested: "team/demo:stable@" + root,
			actual:    "team/other@" + root,
			want:      false,
		},
		{
			name:      "malformed digest",
			requested: "team/demo@sha256:not-a-digest",
			actual:    "team/demo@sha256:not-a-digest",
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appleImageReferencesCompatible(tc.requested, tc.actual); got != tc.want {
				t.Fatalf("appleImageReferencesCompatible(%q, %q) = %v, want %v", tc.requested, tc.actual, got, tc.want)
			}
		})
	}
}
