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
	platformPinningRoot    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	platformPinningVariant = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	platformPinningID      = "3333333333333333333333333333333333333333333333333333333333333333"
)

func platformPinningAppleImageJSON(variants []map[string]any) []byte {
	if len(variants) == 1 {
		variants = append(variants, map[string]any{
			"platform": map[string]any{"os": "linux", "architecture": "amd64"},
			"digest":   "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		})
	}
	return platformPinningAppleImageJSONForRoot(platformPinningRoot, variants)
}

func platformPinningAppleImageJSONForRoot(root string, variants []map[string]any) []byte {
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

type platformPinningRunner struct {
	mu sync.Mutex

	inspectJSON      []byte
	replacementJSON  []byte
	afterPullJSON    []byte
	mutateAfterFirst bool
	inspectCalls     int
	pullCalls        int
	runImage         string
	platform         string
	creation         string
	calls            [][]string
}

func (r *platformPinningRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
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
		for i, arg := range args {
			if arg == "--platform" && i+1 < len(args) {
				r.platform = args[i+1]
			}
			if arg == "--label" && i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"=") {
				r.creation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		image := stripImageDigest(r.runImage)
		if image == "" {
			image = "redis:7-alpine"
		}
		platform := r.platform
		if platform == "" {
			platform = "linux/amd64"
		}
		return platformPinningContainerJSON(image, platformPinningRoot, platform, r.creation), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *platformPinningRunner) state() (runImage string, inspectCalls, pullCalls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage, r.inspectCalls, r.pullCalls
}

func platformPinningContainerJSON(image, digest, platform string, creation ...string) []byte {
	parts := strings.Split(platform, "/")
	platformJSON := map[string]string{}
	if len(parts) > 0 {
		platformJSON["os"] = parts[0]
	}
	if len(parts) > 1 {
		platformJSON["architecture"] = parts[1]
	}
	if len(parts) > 2 {
		platformJSON["variant"] = parts[2]
	}
	labels := map[string]string{managedLabel: "true", sessionLabel: sessionID()}
	gen := "0123456789abcdef"
	if len(creation) > 0 && creation[0] != "" {
		gen = creation[0]
	}
	labels[creationLabel] = gen
	data, _ := json.Marshal([]any{map[string]any{
		"id": "myctr",
		"configuration": map[string]any{
			"id": "myctr",
			"image": map[string]any{
				"reference":  image,
				"descriptor": map[string]string{"digest": digest},
			},
			"platform": platformJSON,
			"labels":   labels,
		},
		"status": map[string]any{"state": "running", "networks": []any{}},
	}})
	return data
}

func callHasPlatform(call []string, platform string) bool {
	for i, arg := range call {
		if arg == "--platform" && i+1 < len(call) && call[i+1] == platform {
			return true
		}
	}
	return false
}

func TestAppleNameDigestIsPinnedEvenWithTag(t *testing.T) {
	input := "redis:7-alpine@" + platformPinningRoot
	r := &platformPinningRunner{inspectJSON: freshAppleImageJSON(platformPinningRoot, platformPinningVariant)}
	identity, exists := (appleEngine{}).parseImageIdentity(platformPinningAppleImageJSON(nil), input, "")
	if !exists || !identity.pinned || identity.mutableAlias {
		t.Fatalf("parsed name@digest identity = %+v, exists = %v, want pinned identity", identity, exists)
	}
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ctr.image.pinned || ctr.image.digest != platformPinningRoot {
		t.Fatalf("run identity = %+v, want pinned root digest", ctr.image)
	}
	if got, _, pulls := r.state(); got != input || pulls != 0 {
		t.Fatalf("run image = %q pulls = %d, want pinned name@digest", got, pulls)
	}
}

func TestAppleNameDigestStaysPinnedWithMutableOptIn(t *testing.T) {
	input := "redis:7-alpine@" + platformPinningRoot
	r := &platformPinningRunner{inspectJSON: freshAppleImageJSON(platformPinningRoot, platformPinningVariant)}
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), WithAllowMutableImageTag(), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ctr.image.pinned || ctr.image.mutableAlias || ctr.image.digest != platformPinningRoot {
		t.Fatalf("identity = %+v, want name@digest to remain pinned", ctr.image)
	}
	if got, _, _ := r.state(); got != input {
		t.Fatalf("run image = %q, want original pinned reference", got)
	}
}

func TestAppleAliasReplacementFailsClosedEvenWithMutablePolicy(t *testing.T) {
	const replacement = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	input := "redis:7-alpine@" + platformPinningRoot
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			r := &platformPinningRunner{
				inspectJSON: freshAppleImageJSON(replacement, replacement),
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

func TestAppleTagReplacementBeforeRunFailsIdentityCheck(t *testing.T) {
	const replacement = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	r := &platformPinningRunner{
		inspectJSON:      freshAppleImageJSON(platformPinningRoot, platformPinningVariant),
		replacementJSON:  freshAppleImageJSON(replacement, replacement),
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

func TestAppleEffectiveDefaultPlatformIsUsedForPullInspectAndRun(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	image := platformPinningAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   platformPinningVariant,
	}})
	r := &platformPinningRunner{inspectJSON: image}
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
			if !callHasPlatform(call, "linux/arm64") {
				t.Errorf("run missing effective platform: %v", call)
			}
		}
		if len(call) >= 2 && call[0] == "image" && call[1] == "pull" {
			sawPull = true
			if !callHasPlatform(call, "linux/arm64") {
				t.Errorf("pull missing effective platform: %v", call)
			}
		}
		if len(call) >= 2 && call[0] == "image" && call[1] == "inspect" && callHasPlatform(call, "linux/arm64") {
			t.Errorf("Apple image inspect must not receive a platform flag: %v", call)
		}
	}
	if !sawRun || !sawPull {
		t.Fatalf("effective platform did not reach pull and run: %v", r.calls)
	}
}

func TestAppleEffectiveDefaultPlatformValidationPrecedesCLI(t *testing.T) {
	for _, platform := range []string{"not-a-platform", "linux", "windows/amd64", "linux/arm64/v7"} {
		t.Run(platform, func(t *testing.T) {
			t.Setenv(defaultPlatformEnv, platform)
			r := &platformPinningRunner{}
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

func TestExplicitPlatformOverridesInvalidAppleDefault(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "not-a-platform")
	image := platformPinningAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   platformPinningVariant,
	}})
	r := &platformPinningRunner{inspectJSON: image}
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{})); err != nil {
		t.Fatalf("explicit platform should override invalid default: %v", err)
	}
}

func TestPullWithUsesAppleEffectiveDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	r := &platformPinningRunner{}
	if err := pullWith(context.Background(), r, appleEngine{}, "redis:7-alpine"); err != nil {
		t.Fatalf("pullWith: %v", err)
	}
	if len(r.calls) != 1 || !callHasPlatform(r.calls[0], "linux/arm64") {
		t.Fatalf("pull calls = %v, want effective platform", r.calls)
	}
}

func TestDockerRunIgnoresAppleDefaultPlatform(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	f := newTestRunner()
	f.imagePresent = true
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if callHasPlatform(f.callWith("run"), "linux/arm64") {
		t.Fatal("Docker consumed Apple's default platform")
	}
}

func TestApplePullMissingFetchesMissingPlatformVariant(t *testing.T) {
	image := platformPinningAppleImageJSON(nil)
	afterPull := platformPinningAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   platformPinningVariant,
	}})
	r := &platformPinningRunner{inspectJSON: image, afterPullJSON: afterPull}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.digest != platformPinningRoot || ctr.image.variantDigest != platformPinningVariant {
		t.Fatalf("resolved identity = %+v, want root and fetched variant", ctr.image)
	}
	if got, _, pulls := r.state(); got != "redis:7-alpine@"+platformPinningRoot || pulls != 1 {
		t.Fatalf("run image=%q pulls=%d, want explicit platform fetch and root run identity", got, pulls)
	}
}

func TestAppleExplicitPlatformRejectsEmptyVariants(t *testing.T) {
	image := platformPinningAppleImageJSON(nil)
	r := &platformPinningRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithPlatform("linux/arm64"),
		withRunner(r), withEngine(applePullNeverBelowPreflight{}))
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

func TestAppleExplicitPlatformRejectsIncompleteVariant(t *testing.T) {
	image := platformPinningAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux"},
		"digest":   platformPinningVariant,
	}})
	r := &platformPinningRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithPlatform("linux/arm64"),
		withRunner(r), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("incomplete variant caused run=%q pulls=%d", runImage, pulls)
	}
}

func TestAppleEffectiveDefaultPlatformValidatesVariants(t *testing.T) {
	t.Setenv(defaultPlatformEnv, "linux/arm64")
	image := platformPinningAppleImageJSON(nil)
	r := &platformPinningRunner{inspectJSON: image}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if runImage, _, pulls := r.state(); runImage != "" || pulls != 0 {
		t.Fatalf("default platform variant check caused run=%q pulls=%d", runImage, pulls)
	}
}

func TestAppleSelectedVariantReplacementFailsBeforeRun(t *testing.T) {
	initial := platformPinningAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   platformPinningVariant,
	}})
	replacementVariant := "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	replacement := platformPinningAppleImageJSONForRoot(platformPinningRoot, []map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   replacementVariant,
	}})
	r := &platformPinningRunner{
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

func TestApplePlatformUsesRootIndexDigestForRunAndReuse(t *testing.T) {
	image := platformPinningAppleImageJSON([]map[string]any{{
		"platform": map[string]any{"os": "linux", "architecture": "arm64"},
		"digest":   platformPinningVariant,
	}})
	r := &platformPinningRunner{inspectJSON: image}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPlatform("linux/arm64"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReference := "redis:7-alpine@" + platformPinningRoot
	if got, _, _ := r.state(); got != wantReference {
		t.Fatalf("run image = %q, want root index reference %q", got, wantReference)
	}
	if ctr.image.digest != platformPinningRoot || ctr.image.platform != "linux/arm64" {
		t.Fatalf("identity = %+v, want root digest and selected platform", ctr.image)
	}
	if ctr.image.variantDigest != platformPinningVariant {
		t.Fatalf("variant digest = %q, want %q", ctr.image.variantDigest, platformPinningVariant)
	}

	actual := imageFromInfo(&engineInfo{
		image:              "redis:7-alpine",
		imageDigest:        platformPinningRoot,
		imageVariantDigest: platformPinningVariant,
		platform:           "linux/arm64",
	})
	if actual.reference != wantReference {
		t.Fatalf("synthesized container reference = %q, want %q", actual.reference, wantReference)
	}
	if !imageIdentitiesCompatible(ctr.image, actual) {
		t.Fatalf("root identity and synthesized container index are incompatible: %+v vs %+v", ctr.image, actual)
	}
}

type dockerIDRunner struct {
	mu           sync.Mutex
	inspect      []byte
	runImage     string
	inspectCalls int
}

func (r *dockerIDRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		r.inspectCalls++
		return r.inspect, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		return []byte(strings.Repeat("a", 64) + "\n"), nil, nil
	case args[0] == "inspect":
		return []byte(fmt.Sprintf(`[{"Id":%q,"Config":{"Image":%q,"Labels":{%q:"true",%q:"0123456789abcdef"}}}]`, strings.Repeat("a", 64), r.runImage, managedLabel, creationLabel)), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestAppleMalformedDescriptorDoesNotFallbackToID(t *testing.T) {
	data := []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"name":"redis:7-alpine","descriptor":{"digest":"not-a-digest"}}}]`, platformPinningID))
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists || identity.pinned {
		t.Fatalf("identity = %+v, exists = %v, want fail-closed malformed descriptor", identity, exists)
	}
}

func TestDockerUnprefixedInspectIDCanonicalizes(t *testing.T) {
	raw := strings.Repeat("a", 64)
	identity, exists := (dockerEngine{}).parseImageIdentity(
		[]byte(`[{"Id":"`+raw+`"}]`), "redis:7-alpine", "")
	if !exists || !identity.pinned || identity.reference != "sha256:"+raw {
		t.Fatalf("identity = %+v exists = %v, want canonical local ID", identity, exists)
	}
	actual := imageFromInfo(&engineInfo{imageID: raw})
	if !actual.pinned || actual.reference != "sha256:"+raw {
		t.Fatalf("container identity = %+v, want canonical local ID", actual)
	}
}

func TestDockerUnprefixedImageIDCanonicalizesOnlyAfterExactMatch(t *testing.T) {
	canonical := "sha256:" + platformPinningID
	input := platformPinningID
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

func TestDockerUnprefixedImageIDRunUsesCanonicalID(t *testing.T) {
	canonical := "sha256:" + platformPinningID
	r := &dockerIDRunner{inspect: []byte(fmt.Sprintf(`[{"Id":%q}]`, canonical))}
	ctr, err := Run(context.Background(), platformPinningID,
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
