package container

import (
	"context"
	"errors"
	"fmt"
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
	f := newDockerTestRunner()
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
	f := newDockerTestRunner()
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

func TestApplePlatformNormalizationPreservesPullSelector(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = false
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/aarch64"), withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.pullCalls != 1 {
		t.Fatalf("pulls = %d, want 1", f.pullCalls)
	}
	var pull []string
	for _, call := range f.calls {
		if len(call) >= 2 && call[0] == "image" && call[1] == "pull" {
			pull = call
		}
	}
	if !hasPlatformArg(pull, "linux/aarch64") {
		t.Errorf("pull did not retain caller platform: %v", f.calls)
	}
	if !hasPlatformArg(f.callWith("run"), "linux/aarch64") {
		t.Errorf("run did not retain caller platform: %v", f.calls)
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
	data := []byte(fmt.Sprintf(`[{"configuration":{"descriptor":{"digest":%q}},"variants":[{"digest":%q,"platform":{"os":"linux","architecture":"arm64"}}]}]`, testImageIdentityOld, testImageIdentityNew))
	if !(appleEngine{}).parseImageExists(data, "linux/arm64") {
		t.Error("want match for linux/arm64")
	}
	if (appleEngine{}).parseImageExists(data, "linux/amd64") {
		t.Error("want mismatch for linux/amd64")
	}
	if !(appleEngine{}).parseImageExists(data, "linux") {
		t.Error("an OS-only selector must accept any Linux architecture")
	}
	if (appleEngine{}).parseImageExists(data, "darwin") {
		t.Error("want mismatch for a different OS")
	}
	if !(appleEngine{}).parseImageExists(data, "") {
		t.Error("empty platform must mean present")
	}
}

func TestApplePlatformVariantMatchingNormalizesAliases(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actual    string
		requested string
	}{
		{name: "aarch64 to arm64", actual: "linux/aarch64", requested: "linux/arm64"},
		{name: "arm64 nil to v8", actual: "linux/arm64", requested: "linux/arm64/v8"},
		{name: "armhf default to arm v7", actual: "linux/arm/v7", requested: "linux/armhf"},
		{name: "armel default to v6", actual: "linux/armel/v6", requested: "linux/armel"},
		{name: "amd64 v1 to amd64", actual: "linux/x86_64/v1", requested: "linux/amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := strings.Split(tc.actual, "/")
			variant := ""
			if len(parts) == 3 {
				variant = fmt.Sprintf(`,"variant":%q`, parts[2])
			}
			data := []byte(fmt.Sprintf(`[{"configuration":{"descriptor":{"digest":%q}},"variants":[{"digest":%q,"platform":{"os":%q,"architecture":%q%s}}]}]`, testImageIdentityOld, testImageIdentityNew, parts[0], parts[1], variant))
			if !(appleEngine{}).parseImageExists(data, tc.requested) {
				t.Fatalf("platform %q did not match inspected %q", tc.requested, tc.actual)
			}
		})
	}
}

func TestAppleParseImageExistsVariantMismatch(t *testing.T) {
	// Variant-less image must not satisfy a variant-pinned request.
	data := []byte(fmt.Sprintf(`[{"configuration":{"descriptor":{"digest":%q}},"variants":[{"digest":%q,"platform":{"os":"linux","architecture":"arm"}}]}]`, testImageIdentityOld, testImageIdentityNew))
	if (appleEngine{}).parseImageExists(data, "linux/arm/v7") {
		t.Error("variant-less arm must not match linux/arm/v7")
	}
	dataV7 := []byte(fmt.Sprintf(`[{"configuration":{"descriptor":{"digest":%q}},"variants":[{"digest":%q,"platform":{"os":"linux","architecture":"arm","variant":"v7"}}]}]`, testImageIdentityOld, testImageIdentityNew))
	if !(appleEngine{}).parseImageExists(dataV7, "linux/arm/v7") {
		t.Error("v7 must match linux/arm/v7")
	}
}

func TestAppleBareArmelSelectorDefaultsToV6(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selector  string
		actual    string
		wantMatch bool
	}{
		{name: "armel matches arm v6", selector: "linux/armel", actual: "linux/arm/v6", wantMatch: true},
		{name: "armel matches explicit armel v6", selector: "linux/armel", actual: "linux/armel/v6", wantMatch: true},
		{name: "armel does not match arm v7", selector: "linux/armel", actual: "linux/arm/v7"},
		{name: "arm still defaults to v7", selector: "linux/arm", actual: "linux/arm/v7", wantMatch: true},
		{name: "armhf still defaults to v7", selector: "linux/armhf", actual: "linux/arm/v7", wantMatch: true},
		{name: "armhf does not match arm v6", selector: "linux/armhf", actual: "linux/arm/v6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := strings.Split(tc.actual, "/")
			data := []byte(fmt.Sprintf(`[{"configuration":{"descriptor":{"digest":%q}},"variants":[{"digest":%q,"platform":{"os":%q,"architecture":%q,"variant":%q}}]}]`, testImageIdentityOld, testImageIdentityNew, parts[0], parts[1], parts[2]))
			if got := (appleEngine{}).parseImageExists(data, tc.selector); got != tc.wantMatch {
				t.Errorf("parseImageExists(%q) against %q = %v, want %v", tc.selector, tc.actual, got, tc.wantMatch)
			}
		})
	}

	want, ok := parseApplePlatformSelector("linux/armel")
	explicit, explicitOK := parseApplePlatformSelector("linux/armel/v6")
	if !ok || !explicitOK || want != explicit || want.variant != "v6" {
		t.Errorf("bare armel = %+v (ok=%v), want same as explicit v6 %+v (ok=%v)", want, ok, explicit, explicitOK)
	}
	for _, selector := range []string{"linux/arm", "linux/armhf"} {
		got, ok := parseApplePlatformSelector(selector)
		if !ok || got.variant != "v7" {
			t.Errorf("%s = %+v (ok=%v), want variant v7", selector, got, ok)
		}
	}
}
