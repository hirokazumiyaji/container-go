package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	issue84ImageRoot    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue84ImageVariant = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func issue84AppleImageFixture() []byte {
	return []byte(`[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","configuration":{"name":"registry.example/team/demo:stable","descriptor":{"digest":"` + issue84ImageRoot + `","mediaType":"application/vnd.oci.image.index.v1+json"}},"variants":[{"platform":{"os":"linux","architecture":"arm64","variant":"v8"},"digest":"` + issue84ImageVariant + `"}]}]`)
}

func TestIssue84AppleCanonicalCustomRegistryReference(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(issue84AppleImageFixture(), "team/demo:stable", "linux/arm64/v8")
	if !exists {
		t.Fatal("Apple image identity was not found")
	}
	if !identity.pinned || identity.repository != "registry.example/team/demo" {
		t.Fatalf("identity = %+v, want pinned custom-registry identity", identity)
	}
	want := "registry.example/team/demo:stable@" + issue84ImageRoot
	if identity.reference != want {
		t.Fatalf("run reference = %q, want %q", identity.reference, want)
	}
}

func TestIssue84AppleNameDigestPreservesCallerPin(t *testing.T) {
	requested := "registry.example/team/demo:stable@" + issue84ImageVariant
	identity, exists := (appleEngine{}).parseImageIdentity(issue84AppleImageFixture(), requested, "linux/arm64/v8")
	if !exists {
		t.Fatal("Apple image identity was not found")
	}
	if !identity.pinned || identity.mutableAlias || identity.rootDigest != issue84ImageRoot {
		t.Fatalf("identity = %+v, want pinned variant with separately tracked root", identity)
	}
	if identity.reference != requested {
		t.Fatalf("run reference = %q, want caller-pinned %q", identity.reference, requested)
	}
	cfg := &config{eng: appleEngine{}, allowMutableImageTag: true}
	resolved, err := cfg.pinImage(requested, identity)
	if err != nil {
		t.Fatalf("pinImage: %v", err)
	}
	if !resolved.pinned || resolved.mutableAlias {
		t.Fatalf("mutable compatibility downgraded caller digest: %+v", resolved)
	}
}

type issue84AppleImageRunner struct {
	exactAddressable bool
	pulls            []string
}

func (r *issue84AppleImageRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		target := args[len(args)-1]
		if strings.Contains(target, "@") && !r.exactAddressable {
			return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: "image not found: " + target}
		}
		return issue84AppleImageFixture(), nil, nil
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "pull" {
		r.pulls = append(r.pulls, args[len(args)-1])
		return nil, nil, nil
	}
	return nil, nil, nil
}

func TestIssue84AppleSyntheticRootFailsClosedWithoutDowngrade(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(issue84AppleImageFixture(), "team/demo:stable", "linux/arm64/v8")
	if !exists || !identity.appleSynthetic {
		t.Fatalf("identity = %+v, want synthetic root", identity)
	}
	runner := &issue84AppleImageRunner{}
	cfg := &config{runner: runner, eng: appleEngine{}}
	_, err := cfg.resolveInspectedImage(context.Background(), "team/demo:stable", "linux/arm64/v8", identity)
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if len(runner.pulls) != 0 {
		t.Fatalf("synthetic root was pulled: %v", runner.pulls)
	}

	fallbackRunner := &issue84AppleImageRunner{}
	fallback := &config{runner: fallbackRunner, eng: appleEngine{}, allowMutableImageTag: true}
	resolved, err := fallback.resolveInspectedImage(context.Background(), "team/demo:stable", "linux/arm64/v8", identity)
	if err != nil {
		t.Fatalf("explicit mutable fallback: %v", err)
	}
	if resolved.pinned || resolved.reference != "team/demo:stable" || len(fallbackRunner.pulls) != 0 {
		t.Fatalf("fallback identity = %+v pulls=%v", resolved, fallbackRunner.pulls)
	}
}

func TestIssue84AppleDigestReferenceCompatibility(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		actual    string
		want      bool
	}{
		{"same custom repository", "team/demo:stable@" + issue84ImageRoot, "registry.example/team/demo:stable@" + issue84ImageRoot, true},
		{"tag is not identity", "team/demo:stable@" + issue84ImageRoot, "registry.example/team/demo:other@" + issue84ImageRoot, true},
		{"different digest", "team/demo:stable@" + issue84ImageRoot, "team/demo:stable@sha256:" + strings.Repeat("c", 64), false},
		{"different repository", "team/demo:stable@" + issue84ImageRoot, "team/other@" + issue84ImageRoot, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appleImageReferencesCompatible(tc.requested, tc.actual); got != tc.want {
				t.Fatalf("appleImageReferencesCompatible(%q, %q) = %v, want %v", tc.requested, tc.actual, got, tc.want)
			}
		})
	}
}

func TestIssue84ApplePinnedDigestMismatchNeverUsesMutableFallback(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(issue84AppleImageFixture(), "team/demo:stable", "linux/arm64/v8")
	if !exists {
		t.Fatal("Apple image identity was not found")
	}
	other := strings.Repeat("c", 64)
	cfg := &config{eng: appleEngine{}, allowMutableImageTag: true}
	_, err := cfg.pinImage("registry.example/team/demo:stable@sha256:"+other, identity)
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want ErrImageIdentityMismatch", err)
	}
}

func TestIssue84AppleIgnoresIncompleteConfigurationPlatform(t *testing.T) {
	data := []byte(`[{"id":"` + strings.Repeat("a", 64) + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + issue84ImageRoot + `"},"platform":{"os":"linux"}}}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("Apple image identity was not found")
	}
	if !identity.pinned {
		t.Fatalf("identity = %+v, want root-descriptor identity", identity)
	}
	if identity.platform != "" {
		t.Fatalf("platform = %q, want the incomplete field to be ignored", identity.platform)
	}
}

func TestIssue84AppleSkipsUnusableVariantEntries(t *testing.T) {
	data := []byte(`[{"id":"` + strings.Repeat("a", 64) + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + issue84ImageRoot + `"}},"variants":[{"digest":"` + issue84ImageVariant + `"},{"platform":{"os":"linux","architecture":"arm64","variant":"v8"},"digest":"` + issue84ImageVariant + `"}]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "linux/arm64/v8")
	if !exists || !identity.pinned {
		t.Fatalf("identity = (%+v, %v), want pinned identity from the usable variant", identity, exists)
	}
	if identity.variantDigest != issue84ImageVariant {
		t.Fatalf("variant digest = %q, want %q", identity.variantDigest, issue84ImageVariant)
	}
}

func TestIssue84AppleIdentitylessDigestNeverUsesMutableFallback(t *testing.T) {
	requested := "registry.example/team/demo:stable@" + issue84ImageRoot
	cfg := &config{eng: appleEngine{}, allowMutableImageTag: true}
	if _, err := cfg.pinImage(requested, imageIdentity{}); !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
}
