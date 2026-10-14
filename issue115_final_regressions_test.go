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
	issue115FinalRoot        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue115FinalVariant     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	issue115FinalOtherRoot   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	issue115FinalReplacement = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func issue115FinalFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/apple_image_inspect_single_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func issue115FinalMultiFixture() []byte {
	return []byte(fmt.Sprintf(`[{"id":"%s","configuration":{"name":"registry.example/team/demo:stable","descriptor":{"digest":"%s","mediaType":"application/vnd.oci.image.index.v1+json"}},"variants":[{"platform":{"os":"linux","architecture":"arm64","variant":"v8"},"digest":"%s"},{"platform":{"os":"linux","architecture":"amd64"},"digest":"%s"}]}]`, strings.TrimPrefix(issue115FinalRoot, "sha256:"), issue115FinalRoot, issue115FinalVariant, issue115FinalOtherRoot))
}

type issue115FinalRunner struct {
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

func (r *issue115FinalRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
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
			root = issue115FinalRoot
		}
		platform := r.postPlatform
		if platform == "" {
			platform = r.runPlatform
		}
		if platform == "" {
			platform = "linux/arm64/v8"
		}
		variant := r.postVariant
		return issue115FinalContainerJSON(r.runImage, root, variant, platform, r.creation), nil, nil
	case args[0] == "delete":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func issue115FinalContainerJSON(image, root, variant, platform, creation string) []byte {
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

func TestIssue115AppleSingleManifestSyntheticIndexDoesNotPullRoot(t *testing.T) {
	image := issue115FinalFixture(t)
	r := &issue115FinalRunner{imageJSON: image}
	_, err := Run(context.Background(), "team/demo:stable",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal for synthetic Apple root", err)
	}
	if len(r.pullTargets) != 0 || r.runImage != "" {
		t.Fatalf("synthetic root was pulled/run: pulls=%v run=%q", r.pullTargets, r.runImage)
	}

	fallback := &issue115FinalRunner{imageJSON: image}
	ctr, err := Run(context.Background(), "team/demo:stable",
		WithName("myctr"), WithAllowMutableImageTag(), withRunner(fallback), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("explicit fallback: %v", err)
	}
	if fallback.runImage != "team/demo:stable" || ctr.image.pinned || len(fallback.pullTargets) != 0 {
		t.Fatalf("fallback identity = %+v run=%q pulls=%v", ctr.image, fallback.runImage, fallback.pullTargets)
	}
}

func TestIssue115AppleNameDigestWithTagIsPinned(t *testing.T) {
	input := "registry.example/team/demo:stable@" + issue115FinalRoot
	r := &issue115FinalRunner{imageJSON: issue115FinalFixture(t), exactAddressable: true}
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ctr.image.pinned || ctr.image.mutableAlias || r.runImage != input {
		t.Fatalf("identity = %+v run=%q, want pinned name:tag@digest", ctr.image, r.runImage)
	}
}

func TestIssue115ApplePreservesCustomDefaultRegistry(t *testing.T) {
	r := &issue115FinalRunner{imageJSON: issue115FinalFixture(t), exactAddressable: true}
	ctr, err := Run(context.Background(), "team/demo:stable",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
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

func TestIssue115ApplePreservesCustomDefaultRegistryForPinnedReference(t *testing.T) {
	fixture := strings.Replace(string(issue115FinalFixture(t)),
		`"name": "registry.example/team/demo:stable"`,
		`"name": "registry.example/team/demo:stable@`+issue115FinalRoot+`"`, 1)
	r := &issue115FinalRunner{imageJSON: []byte(fixture), exactAddressable: true}
	input := "team/demo:stable@" + issue115FinalRoot
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
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

func TestIssue115AppleConfigurationPlatformAndVariantIdentity(t *testing.T) {
	data := issue115FinalContainerJSON("registry.example/team/demo:stable", issue115FinalRoot, issue115FinalVariant, "linux/arm64/v8", "0123456789abcdef")
	info, err := (appleEngine{}).parseInspect(data, "myctr")
	if err != nil {
		t.Fatal(err)
	}
	if info.platform != "linux/arm64/v8" {
		t.Fatalf("platform = %q, want configuration.platform", info.platform)
	}
	identity := imageFromInfo(info)
	if !identity.pinned || identity.variantDigest != issue115FinalVariant || identity.digest != issue115FinalRoot {
		t.Fatalf("identity = %+v, want root and selected variant identities", identity)
	}
	other := identity
	other.platform = "linux/amd64"
	if imageIdentitiesCompatible(identity, other) {
		t.Fatal("different Apple platform variants were treated as compatible")
	}
}

func TestIssue115AppleUnselectedPlatformMetadataDoesNotBlockReuse(t *testing.T) {
	requested := imageIdentity{
		reference:  "registry.example/team/demo:stable@" + issue115FinalRoot,
		digest:     issue115FinalRoot,
		rootDigest: issue115FinalRoot,
		repository: "registry.example/team/demo",
		pinned:     true,
	}
	actual := imageFromInfo(&engineInfo{
		image:              "registry.example/team/demo:stable",
		imageDigest:        issue115FinalRoot,
		imageVariantDigest: issue115FinalVariant,
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

func TestIssue115RejectsKnownDifferentDockerImageIDs(t *testing.T) {
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

func TestIssue115ImageMissingReasonIsAnchored(t *testing.T) {
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

func TestIssue115ApplePostCreateIdentityReplacementRollsBack(t *testing.T) {
	r := &issue115FinalRunner{
		imageJSON:        issue115FinalMultiFixture(),
		exactAddressable: true,
		postRoot:         issue115FinalReplacement,
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
