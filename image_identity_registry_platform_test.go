package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	finalRoot        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	finalVariant     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	finalOtherRoot   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	finalReplacement = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func finalFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/apple_image_inspect_single_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func finalMultiFixture() []byte {
	return []byte(fmt.Sprintf(`[{"id":"%s","configuration":{"name":"registry.example/team/demo:stable","descriptor":{"digest":"%s","mediaType":"application/vnd.oci.image.index.v1+json"}},"variants":[{"platform":{"os":"linux","architecture":"arm64","variant":"v8"},"digest":"%s"},{"platform":{"os":"linux","architecture":"amd64"},"digest":"%s"}]}]`, strings.TrimPrefix(finalRoot, "sha256:"), finalRoot, finalVariant, finalOtherRoot))
}

type finalRunner struct {
	mu sync.Mutex

	imageJSON         []byte
	exactAddressable  bool
	postRoot          string
	postVariant       string
	postPlatform      string
	runImage          string
	runPlatform       string
	creation          string
	pullTargets       []string
	deleteCalls       int
	containerInspects int
}

func (r *finalRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		target := args[len(args)-1]
		if strings.Contains(target, "@") && !r.exactAddressable {
			return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: "image not found: " + target}
		}
		return r.imageJSON, nil, nil
	case len(args) >= 2 && args[0] == "image" && args[1] == "pull":
		r.pullTargets = append(r.pullTargets, args[len(args)-1])
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		for i, arg := range args {
			switch {
			case arg == "--platform" && i+1 < len(args):
				r.runPlatform = args[i+1]
			case arg == "--label" && i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"="):
				r.creation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		r.containerInspects++
		root := r.postRoot
		if root == "" {
			root = finalRoot
		}
		platform := r.postPlatform
		if platform == "" {
			platform = r.runPlatform
		}
		if platform == "" {
			platform = "linux/arm64/v8"
		}
		variant := r.postVariant
		return finalContainerJSON(r.runImage, root, variant, platform, r.creation), nil, nil
	case args[0] == "delete":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func finalContainerJSON(image, root, variant, platform, creation string) []byte {
	if image == "" {
		image = "registry.example/team/demo:stable"
	}
	parts := strings.Split(platform, "/")
	platformValue := map[string]string{}
	if len(parts) > 0 {
		platformValue["os"] = parts[0]
	}
	if len(parts) > 1 {
		platformValue["architecture"] = parts[1]
	}
	if len(parts) > 2 {
		platformValue["variant"] = parts[2]
	}
	imageValue := map[string]any{
		"reference":  stripImageDigest(image),
		"descriptor": map[string]string{"digest": root},
	}
	if variant != "" {
		imageValue["variantDigest"] = variant
	}
	labels := map[string]string{managedLabel: "true", sessionLabel: sessionID()}
	if creation != "" {
		labels[creationLabel] = creation
	}
	data, _ := json.Marshal([]any{map[string]any{
		"id": "myctr",
		"configuration": map[string]any{
			"id":       "myctr",
			"image":    imageValue,
			"platform": platformValue,
			"labels":   labels,
		},
		"status": map[string]any{"state": "running", "networks": []any{}},
	}})
	return data
}

func TestAppleSingleManifestSyntheticIndexDoesNotPullRoot(t *testing.T) {
	image := finalFixture(t)
	r := &finalRunner{imageJSON: image}
	_, err := Run(context.Background(), "team/demo:stable",
		WithName("myctr"), WithPlatform("linux/arm64/v8"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal for synthetic Apple root", err)
	}
	if len(r.pullTargets) != 0 || r.runImage != "" {
		t.Fatalf("synthetic root was pulled/run: pulls=%v run=%q", r.pullTargets, r.runImage)
	}

	fallback := &finalRunner{imageJSON: image}
	ctr, err := Run(context.Background(), "team/demo:stable",
		WithName("myctr"), WithPlatform("linux/arm64/v8"), WithAllowMutableImageTag(), withRunner(fallback), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("explicit fallback: %v", err)
	}
	if fallback.runImage != "team/demo:stable" || ctr.image.pinned || len(fallback.pullTargets) != 0 {
		t.Fatalf("fallback identity = %+v run=%q pulls=%v", ctr.image, fallback.runImage, fallback.pullTargets)
	}
}

func TestAppleNameDigestWithTagIsPinned(t *testing.T) {
	input := "registry.example/team/demo:stable@" + finalRoot
	r := &finalRunner{imageJSON: finalFixture(t), exactAddressable: true}
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), WithPlatform("linux/arm64/v8"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ctr.image.pinned || ctr.image.mutableAlias || r.runImage != input {
		t.Fatalf("identity = %+v run=%q, want pinned name:tag@digest", ctr.image, r.runImage)
	}
}

func TestApplePreservesCustomDefaultRegistry(t *testing.T) {
	r := &finalRunner{imageJSON: finalFixture(t), exactAddressable: true}
	ctr, err := Run(context.Background(), "team/demo:stable",
		WithName("myctr"), WithPlatform("linux/arm64/v8"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(r.runImage, "registry.example/team/demo:stable@") {
		t.Fatalf("run image = %q, want backend canonical registry", r.runImage)
	}
	if ctr.image.repository != "registry.example/team/demo" {
		t.Fatalf("repository = %q, want custom default registry", ctr.image.repository)
	}
}

func TestApplePreservesCustomDefaultRegistryForPinnedReference(t *testing.T) {
	fixture := strings.Replace(string(finalFixture(t)),
		`"name": "registry.example/team/demo:stable"`,
		`"name": "registry.example/team/demo:stable@`+finalRoot+`"`, 1)
	r := &finalRunner{imageJSON: []byte(fixture), exactAddressable: true}
	input := "team/demo:stable@" + finalRoot
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), WithPlatform("linux/arm64/v8"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(r.runImage, "registry.example/team/demo:stable@") {
		t.Fatalf("run image = %q, want canonical pinned reference", r.runImage)
	}
	if ctr.image.repository != "registry.example/team/demo" || !ctr.image.pinned {
		t.Fatalf("identity = %+v, want pinned custom-registry identity", ctr.image)
	}
}

func TestAppleConfigurationPlatformAndVariantIdentity(t *testing.T) {
	data := finalContainerJSON("registry.example/team/demo:stable", finalRoot, finalVariant, "linux/arm64/v8", "0123456789abcdef")
	info, err := (appleEngine{}).parseInspect(data, "myctr")
	if err != nil {
		t.Fatal(err)
	}
	if info.platform != "linux/arm64/v8" {
		t.Fatalf("platform = %q, want configuration.platform", info.platform)
	}
	identity := imageFromInfo(info)
	if !identity.pinned || identity.variantDigest != finalVariant || identity.digest != finalRoot {
		t.Fatalf("identity = %+v, want root and selected variant identities", identity)
	}
	other := identity
	other.platform = "linux/amd64"
	if imageIdentitiesCompatible(identity, other) {
		t.Fatal("different Apple platform variants were treated as compatible")
	}
}

func TestAppleUnselectedPlatformMetadataDoesNotBlockReuse(t *testing.T) {
	requested := imageIdentity{
		reference:  "registry.example/team/demo:stable@" + finalRoot,
		digest:     finalRoot,
		rootDigest: finalRoot,
		repository: "registry.example/team/demo",
		pinned:     true,
	}
	actual := imageFromInfo(&engineInfo{
		image:              "registry.example/team/demo:stable",
		imageDigest:        finalRoot,
		imageVariantDigest: finalVariant,
		platform:           "linux/arm64/v8",
	})
	if !requestedImageIdentitiesCompatible(requested, actual) {
		t.Fatal("an unselected platform made an otherwise matching Apple identity incompatible")
	}
	requested.platform = "linux/arm64/v8"
	withoutActualPlatform := actual
	withoutActualPlatform.platform = ""
	withoutActualPlatform.variantDigest = ""
	if imageIdentitiesCompatible(requested, withoutActualPlatform) {
		t.Fatal("an explicitly selected platform was accepted without actual metadata")
	}
}

func TestRejectsKnownDifferentDockerImageIDs(t *testing.T) {
	root := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := imageIdentity{reference: "registry.example/team/demo@" + root, digest: root, rootDigest: root, repository: "registry.example/team/demo", id: "sha256:" + strings.Repeat("1", 64), pinned: true}
	b := a
	b.id = "sha256:" + strings.Repeat("2", 64)
	if imageIdentitiesCompatible(a, b) {
		t.Fatal("different Docker image IDs were treated as compatible")
	}
	data := []byte(`[{"Id":"sha256:` + strings.Repeat("3", 64) + `"}]`)
	requested := "sha256:" + strings.Repeat("1", 64)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, requested, "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v exists=%v, want known-different ID mismatch", identity, exists)
	}
}

func TestImageMissingReasonIsAnchored(t *testing.T) {
	imageName := "not-found-image"
	args := []string{"image", "inspect", imageName}
	if (appleEngine{}).imageMissing(&cli.CLIError{Binary: "container", Args: args, Stderr: "failed to inspect " + imageName}) {
		t.Fatal("image name containing not-found triggered Apple absence classification")
	}
	if (dockerEngine{}).imageMissing(&cli.CLIError{Binary: "docker", Args: args, Stderr: "failed to inspect " + imageName}) {
		t.Fatal("image name containing not-found triggered Docker absence classification")
	}
	if !(appleEngine{}).imageMissing(&cli.CLIError{Binary: "container", Args: args, Stderr: "image not found: " + imageName}) {
		t.Fatal("anchored Apple image absence reason was not recognized")
	}
	if (appleEngine{}).imageMissing(&cli.CLIError{Binary: "docker", Args: args, Stderr: "image not found: " + imageName}) {
		t.Fatal("error from another backend was classified as Apple absence")
	}
}

func TestApplePostCreateIdentityReplacementRollsBack(t *testing.T) {
	r := &finalRunner{
		imageJSON:        finalMultiFixture(),
		exactAddressable: true,
		postRoot:         finalReplacement,
	}
	_, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want post-create identity mismatch", err)
	}
	if r.deleteCalls != 1 || r.runImage == "" {
		t.Fatalf("replacement was not rolled back: run=%q deletes=%d", r.runImage, r.deleteCalls)
	}
}
