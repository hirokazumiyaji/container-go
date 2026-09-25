package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureImagePassesPlatformToPull(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	cfg := &config{runner: f, eng: dockerEngine{}, pullPolicy: PullAlways, platform: "linux/amd64", name: "myctr"}
	if err := cfg.ensureImage(context.Background(), "redis:7-alpine"); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}
	var pullArgs []string
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "pull" {
			pullArgs = c
		}
	}
	if pullArgs == nil {
		t.Fatal("no pull call")
	}
	joined := strings.Join(pullArgs, " ")
	if !strings.Contains(joined, "--platform linux/amd64") {
		t.Errorf("pull args missing platform: %v", pullArgs)
	}
}

func TestEnsureImagePassesPlatformToInspect(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	cfg := &config{runner: f, eng: dockerEngine{}, pullPolicy: PullNever, platform: "linux/amd64", name: "myctr"}
	if _, err := imageExists(context.Background(), f, dockerEngine{}, "redis:7-alpine", "linux/amd64"); err != nil {
		t.Fatalf("imageExists: %v", err)
	}
	found := false
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "image" && c[1] == "inspect" {
			if strings.Contains(strings.Join(c, " "), "--platform linux/amd64") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("inspect missing platform: %v", f.calls)
	}
	_ = cfg
}

func TestPullNeverWithPlatformReportsMissing(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = false
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithPlatform("linux/amd64"),
		withRunner(f), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("error = %v, want ErrImageNotFound", err)
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("error %v does not mention platform", err)
	}
	if f.callWith("run") != nil {
		t.Error("run was called despite missing image")
	}
}

func TestPlatformEmptyPreservesCallCounts(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = ctr
	// image inspect + run, no platform flags.
	for _, c := range f.calls {
		for _, a := range c {
			if a == "--platform" {
				t.Fatalf("unexpected --platform in %v", c)
			}
		}
	}
}

func hasPlatformArg(args []string, platform string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--platform" && args[i+1] == platform {
			return true
		}
	}
	return false
}

func TestRunResolvesDefaultPlatformForInspectPullAndRun(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	f := newTestRunner()
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing), withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var inspect, pull, run []string
	for _, call := range f.calls {
		switch {
		case len(call) >= 2 && call[0] == "image" && call[1] == "inspect":
			inspect = call
		case len(call) >= 2 && call[0] == "image" && call[1] == "pull":
			pull = call
		case len(call) > 0 && call[0] == "run":
			run = call
		}
	}
	if inspect == nil || pull == nil || run == nil {
		t.Fatalf("inspect/pull/run calls missing: %v", f.calls)
	}
	// Apple image inspect has no --platform option; imageExists receives the
	// effective platform and applies it while parsing returned variants.
	if hasPlatformArg(inspect, "linux/arm64") {
		t.Fatalf("Apple image inspect unexpectedly used --platform: %v", inspect)
	}
	if !hasPlatformArg(pull, "linux/arm64") || !hasPlatformArg(run, "linux/arm64") {
		t.Fatalf("effective platform missing from pull/run: %v", f.calls)
	}
}

func TestExplicitPlatformOverridesDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	f := newTestRunner()
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/amd64"), withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, call := range f.calls {
		if hasPlatformArg(call, "linux/arm64") {
			t.Fatalf("default platform overrode explicit platform: %v", call)
		}
	}
	if !hasPlatformArg(f.callWith("run"), "linux/amd64") {
		t.Fatalf("run args do not use explicit platform: %v", f.callWith("run"))
	}
}

func TestPullWithResolvesDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	f := newTestRunner()
	if err := pullWith(context.Background(), f, appleEngine{}, "redis:7-alpine"); err != nil {
		t.Fatalf("pullWith: %v", err)
	}
	var pull []string
	for _, call := range f.calls {
		if len(call) >= 2 && call[0] == "image" && call[1] == "pull" {
			pull = call
		}
	}
	if f.pullCalls != 1 || !hasPlatformArg(pull, "linux/arm64") {
		t.Fatalf("pull did not use effective platform: calls=%v", f.calls)
	}
}

func TestPullWithAppliesAppleCapabilityValidation(t *testing.T) {
	for _, platform := range []string{"linux", "windows/amd64", "linux/arm64/v7"} {
		t.Run(platform, func(t *testing.T) {
			t.Setenv(defaultPlatformEnv, platform)
			f := newTestRunner()
			err := pullWith(context.Background(), f, appleEngine{}, "redis:7-alpine")
			if err == nil {
				t.Fatalf("pullWith accepted Apple default platform %q", platform)
			}
			if len(f.calls) != 0 {
				t.Fatalf("invalid Apple default reached CLI: %v", f.calls)
			}
		})
	}
}

func TestExplicitPlatformIgnoresInvalidAppleDefault(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "not-a-platform")
	f := newTestRunner()
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/amd64"), withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("explicit platform should take precedence over invalid default: %v", err)
	}
}

func TestDockerIgnoresAppleDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	f := newTestRunner()
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Docker unexpectedly consumed Apple default platform: %v", err)
	}
	for _, call := range f.calls {
		if hasPlatformArg(call, "linux/arm64") {
			t.Fatalf("Docker unexpectedly used Apple default platform: %v", call)
		}
	}
}

func TestDockerKeepsBarePlatformCompatible(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux"), withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Docker rejected bare platform: %v", err)
	}
	if !hasPlatformArg(f.callWith("run"), "linux") {
		t.Fatalf("run args missing bare platform: %v", f.callWith("run"))
	}
}

func TestDockerKeepsGenericVariantPlatforms(t *testing.T) {
	for _, platform := range []string{"linux/arm64/v7", "linux/amd64/v2", "linux/unknown"} {
		t.Run(platform, func(t *testing.T) {
			f := newTestRunner()
			f.imagePresent = true
			if _, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPlatform(platform), withRunner(f), withEngine(dockerEngine{})); err != nil {
				t.Fatalf("Docker rejected generic platform %q: %v", platform, err)
			}
			if !hasPlatformArg(f.callWith("run"), platform) {
				t.Fatalf("run args missing platform %q: %v", platform, f.callWith("run"))
			}
		})
	}
}

func TestAppleParseImageExistsPlatform(t *testing.T) {
	data := []byte(`[{"variants":[{"platform":{"os":"linux","architecture":"arm64"}}]}]`)
	if !(appleEngine{}).parseImageExists(data, "linux/arm64") {
		t.Error("want match for linux/arm64")
	}
	if (appleEngine{}).parseImageExists(data, "linux/amd64") {
		t.Error("want mismatch for linux/amd64")
	}
	if !(appleEngine{}).parseImageExists(data, "") {
		t.Error("empty platform must mean present")
	}
}

func TestAppleParseImageExistsVariantMismatch(t *testing.T) {
	// Variant-less image must not satisfy a variant-pinned request.
	data := []byte(`[{"variants":[{"platform":{"os":"linux","architecture":"arm"}}]}]`)
	if (appleEngine{}).parseImageExists(data, "linux/arm/v7") {
		t.Error("variant-less arm must not match linux/arm/v7")
	}
	dataV7 := []byte(`[{"variants":[{"platform":{"os":"linux","architecture":"arm","variant":"v7"}}]}]`)
	if !(appleEngine{}).parseImageExists(dataV7, "linux/arm/v7") {
		t.Error("v7 must match linux/arm/v7")
	}
}
