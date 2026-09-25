package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

const (
	issue115ReviewRoot    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	issue115ReviewVariant = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	issue115ReviewID      = "3333333333333333333333333333333333333333333333333333333333333333"
)

func issue115ReviewAppleImageJSON(variants []map[string]any) []byte {
	return issue115ReviewAppleImageJSONForRoot(issue115ReviewRoot, variants)
}

func issue115ReviewAppleImageJSONForRoot(root string, variants []map[string]any) []byte {
	data := map[string]any{
		"id": strings.TrimPrefix(root, "sha256:"),
		"configuration": map[string]any{
			"name": "redis:7-alpine",
			"descriptor": map[string]any{
				"digest": root,
			},
		},
		"variants": variants,
	}
	out, _ := json.Marshal([]any{data})
	return out
}

type issue115ReviewRunner struct {
	mu sync.Mutex

	inspectJSON      []byte
	replacementJSON  []byte
	afterPullJSON    []byte
	mutateAfterFirst bool
	inspectCalls     int
	pullCalls        int
	runImage         string
	calls            [][]string
}

func (r *issue115ReviewRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		r.inspectCalls++
		if r.mutateAfterFirst && r.inspectCalls > 1 && r.replacementJSON != nil {
			return r.replacementJSON, nil, nil
		}
		return r.inspectJSON, nil, nil
	case len(args) >= 2 && args[0] == "image" && args[1] == "pull":
		r.pullCalls++
		if r.afterPullJSON != nil {
			r.inspectJSON = r.afterPullJSON
		}
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		return []byte("myctr\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *issue115ReviewRunner) state() (runImage string, inspectCalls, pullCalls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage, r.inspectCalls, r.pullCalls
}

func issue115ReviewCallHasPlatform(call []string, platform string) bool {
	for i, arg := range call {
		if arg == "--platform" && i+1 < len(call) && call[i+1] == platform {
			return true
		}
	}
	return false
}

func TestIssue115AppleNameDigestAliasFailsClosed(t *testing.T) {
	input := "redis:7-alpine@" + issue115ReviewRoot
	r := &issue115ReviewRunner{inspectJSON: issue115ReviewAppleImageJSON(nil)}
	identity, exists := (appleEngine{}).parseImageIdentity(issue115ReviewAppleImageJSON(nil), input, "")
	if !exists || !identity.mutableAlias || !identity.pinned {
		t.Fatalf("parsed alias identity = %+v, exists = %v, want explicit mutable-alias state", identity, exists)
	}
	_, err := Run(context.Background(), input,
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable for mutable Apple alias", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("alias reached run=%q pulls=%d", runImage, pulls)
	}
}

func TestIssue115AppleNameDigestAliasRequiresExplicitMutablePolicy(t *testing.T) {
	input := "redis:7-alpine@" + issue115ReviewRoot
	r := &issue115ReviewRunner{inspectJSON: issue115ReviewAppleImageJSON(nil)}
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), WithAllowMutableImageTag(), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.pinned || ctr.image.digest != issue115ReviewRoot {
		t.Fatalf("mutable alias identity = %+v, want unpinned reference with retained content digest", ctr.image)
	}
	if got, _, _ := r.state(); got != input {
		t.Fatalf("run image = %q, want original mutable alias %q", got, input)
	}
}

func TestIssue115AppleAliasReplacementFailsClosedEvenWithMutablePolicy(t *testing.T) {
	const replacement = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	input := "redis:7-alpine@" + issue115ReviewRoot
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			r := &issue115ReviewRunner{
				inspectJSON: issue115ReviewAppleImageJSONForRoot(replacement, nil),
			}
			opts := []Option{WithName("myctr"), withRunner(r), withEngine(appleEngine{})}
			if allow {
				opts = append(opts, WithAllowMutableImageTag())
			}
			_, err := Run(context.Background(), input, opts...)
			if !errors.Is(err, ErrImageIdentityMismatch) {
				t.Fatalf("error = %v, want ErrImageIdentityMismatch for replaced alias", err)
			}
			if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
				t.Fatalf("replaced alias reached run=%q pulls=%d", runImage, pulls)
			}
		})
	}
}

func TestIssue115AppleTagReplacementBeforeRunFailsIdentityCheck(t *testing.T) {
	const replacement = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	r := &issue115ReviewRunner{
		inspectJSON:      issue115ReviewAppleImageJSON(nil),
		replacementJSON:  issue115ReviewAppleImageJSONForRoot(replacement, nil),
		mutateAfterFirst: true,
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want ErrImageIdentityMismatch for tag replacement", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("replaced tag reached run=%q pulls=%d", runImage, pulls)
	}
}

func TestIssue115AppleEffectiveDefaultPlatformIsUsedForPullInspectAndRun(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	image := issue115ReviewAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   issue115ReviewVariant,
	}})
	r := &issue115ReviewRunner{inspectJSON: image}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullAlways), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.platform != "linux/arm64" {
		t.Fatalf("identity platform = %q, want effective default", ctr.image.platform)
	}
	var sawRun, sawPull bool
	for _, call := range r.calls {
		if len(call) > 0 && call[0] == "run" {
			sawRun = true
			if !issue115ReviewCallHasPlatform(call, "linux/arm64") {
				t.Errorf("run missing effective platform: %v", call)
			}
		}
		if len(call) >= 2 && call[0] == "image" && call[1] == "pull" {
			sawPull = true
			if !issue115ReviewCallHasPlatform(call, "linux/arm64") {
				t.Errorf("pull missing effective platform: %v", call)
			}
		}
		if len(call) >= 2 && call[0] == "image" && call[1] == "inspect" && issue115ReviewCallHasPlatform(call, "linux/arm64") {
			t.Errorf("Apple image inspect must not receive a platform flag: %v", call)
		}
	}
	if !sawRun || !sawPull {
		t.Fatalf("effective platform did not reach pull and run: %v", r.calls)
	}
}

func TestIssue115AppleEffectiveDefaultPlatformValidationPrecedesCLI(t *testing.T) {
	for _, platform := range []string{"not-a-platform", "linux", "windows/amd64", "linux/arm64/v7"} {
		t.Run(platform, func(t *testing.T) {
			t.Setenv(defaultPlatformEnv, platform)
			r := &issue115ReviewRunner{}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
			if err == nil {
				t.Fatalf("accepted invalid Apple default platform %q", platform)
			}
			if len(r.calls) != 0 {
				t.Fatalf("invalid effective platform reached CLI: %v", r.calls)
			}
		})
	}
}

func TestIssue115ExplicitPlatformOverridesInvalidAppleDefault(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "not-a-platform")
	image := issue115ReviewAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   issue115ReviewVariant,
	}})
	r := &issue115ReviewRunner{inspectJSON: image}
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{})); err != nil {
		t.Fatalf("explicit platform should override invalid default: %v", err)
	}
}

func TestIssue115PullWithUsesAppleEffectiveDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	r := &issue115ReviewRunner{}
	if err := pullWith(context.Background(), r, appleEngine{}, "redis:7-alpine"); err != nil {
		t.Fatalf("pullWith: %v", err)
	}
	if len(r.calls) != 1 || !issue115ReviewCallHasPlatform(r.calls[0], "linux/arm64") {
		t.Fatalf("pull calls = %v, want effective platform", r.calls)
	}
}

func TestIssue115DockerIgnoresAppleDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	f := newTestRunner()
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if issue115ReviewCallHasPlatform(f.callWith("run"), "linux/arm64") {
		t.Fatal("Docker consumed Apple's default platform")
	}
}

func TestIssue115ApplePullMissingFetchesMissingPlatformVariant(t *testing.T) {
	image := issue115ReviewAppleImageJSON(nil)
	afterPull := issue115ReviewAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   issue115ReviewVariant,
	}})
	r := &issue115ReviewRunner{inspectJSON: image, afterPullJSON: afterPull}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.digest != issue115ReviewRoot || ctr.image.variantDigest != issue115ReviewVariant {
		t.Fatalf("resolved identity = %+v, want root and fetched variant", ctr.image)
	}
	if got, _, pulls := r.state(); got != "redis:7-alpine@"+issue115ReviewRoot || pulls != 1 {
		t.Fatalf("run image=%q pulls=%d, want explicit platform fetch and root run identity", got, pulls)
	}
}

func TestIssue115AppleExplicitPlatformRejectsEmptyVariants(t *testing.T) {
	image := issue115ReviewAppleImageJSON(nil)
	r := &issue115ReviewRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithPlatform("linux/arm64"),
		withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("empty variant list was reported as identity-unavailable: %v", err)
	}
	runImage, _, pulls := r.state()
	if runImage != "" || pulls != 0 {
		t.Fatalf("empty variant list caused run=%q pulls=%d", runImage, pulls)
	}
}

func TestIssue115AppleExplicitPlatformRejectsIncompleteVariant(t *testing.T) {
	image := issue115ReviewAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux"},
		"digest":   issue115ReviewVariant,
	}})
	r := &issue115ReviewRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithPlatform("linux/arm64"),
		withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("incomplete variant caused run=%q pulls=%d", runImage, pulls)
	}
}

func TestIssue115AppleEffectiveDefaultPlatformValidatesVariants(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	image := issue115ReviewAppleImageJSON(nil)
	r := &issue115ReviewRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("default platform variant check caused run=%q pulls=%d", runImage, pulls)
	}
}

func TestIssue115AppleSelectedVariantReplacementFailsBeforeRun(t *testing.T) {
	initial := issue115ReviewAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   issue115ReviewVariant,
	}})
	replacementVariant := "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	replacement := issue115ReviewAppleImageJSONForRoot(issue115ReviewRoot, []map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   replacementVariant,
	}})
	r := &issue115ReviewRunner{
		inspectJSON:      initial,
		replacementJSON:  replacement,
		mutateAfterFirst: true,
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want ErrImageIdentityMismatch for replaced variant", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("replaced variant reached run=%q pulls=%d", runImage, pulls)
	}
}

func TestIssue115ApplePlatformUsesRootIndexDigestForRunAndReuse(t *testing.T) {
	image := issue115ReviewAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   issue115ReviewVariant,
	}})
	r := &issue115ReviewRunner{inspectJSON: image}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReference := "redis:7-alpine@" + issue115ReviewRoot
	if got, _, _ := r.state(); got != wantReference {
		t.Fatalf("run image = %q, want root index reference %q", got, wantReference)
	}
	if ctr.image.digest != issue115ReviewRoot || ctr.image.platform != "linux/arm64" {
		t.Fatalf("identity = %+v, want root digest and selected platform", ctr.image)
	}
	if ctr.image.variantDigest != issue115ReviewVariant {
		t.Fatalf("variant digest = %q, want %q", ctr.image.variantDigest, issue115ReviewVariant)
	}

	actual := imageFromInfo(&engineInfo{
		image:       "redis:7-alpine",
		imageDigest: issue115ReviewRoot,
	})
	if actual.reference != wantReference {
		t.Fatalf("synthesized container reference = %q, want %q", actual.reference, wantReference)
	}
	if !imageIdentitiesCompatible(ctr.image, actual) {
		t.Fatalf("root identity and synthesized container index are incompatible: %+v vs %+v", ctr.image, actual)
	}
}

type issue115DockerIDRunner struct {
	mu           sync.Mutex
	inspect      []byte
	runImage     string
	inspectCalls int
}

func (r *issue115DockerIDRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		r.inspectCalls++
		return r.inspect, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		return []byte("myctr\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestIssue115AppleMalformedDescriptorDoesNotFallbackToID(t *testing.T) {
	data := []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"name":"redis:7-alpine","descriptor":{"digest":"not-a-digest"}}}]`, issue115ReviewID))
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists || identity.pinned {
		t.Fatalf("identity = %+v, exists = %v, want fail-closed malformed descriptor", identity, exists)
	}
}

func TestIssue115DockerUnprefixedImageIDCanonicalizesOnlyAfterExactMatch(t *testing.T) {
	canonical := "sha256:" + issue115ReviewID
	input := issue115ReviewID
	matching := []byte(fmt.Sprintf(`[{"Id":%q}]`, canonical))
	identity, exists := (dockerEngine{}).parseImageIdentity(matching, input, "")
	if !exists || !identity.pinned || identity.reference != canonical {
		t.Fatalf("identity = %+v, exists = %v, want canonical %q", identity, exists, canonical)
	}

	mismatched := []byte(`[{"Id":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
	identity, exists = (dockerEngine{}).parseImageIdentity(mismatched, input, "")
	if !exists || identity.pinned {
		t.Fatalf("identity = %+v, exists = %v, want fail-closed ID mismatch", identity, exists)
	}
}

func TestIssue115DockerUnprefixedImageIDRunUsesCanonicalID(t *testing.T) {
	canonical := "sha256:" + issue115ReviewID
	r := &issue115DockerIDRunner{inspect: []byte(fmt.Sprintf(`[{"Id":%q}]`, canonical))}
	ctr, err := Run(context.Background(), issue115ReviewID,
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.reference != canonical || r.runImage != canonical {
		t.Fatalf("run identity = %q, runner image = %q, want %q", ctr.image.reference, r.runImage, canonical)
	}
	if r.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want initial and exact-address checks", r.inspectCalls)
	}
}
