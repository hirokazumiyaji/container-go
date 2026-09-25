package container

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestIssue84AppleImageIdentityRetainsRootAndVariant(t *testing.T) {
	data, err := os.ReadFile("testdata/apple_image_inspect_v1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "linux/arm64/v8")
	if !exists || !identity.pinned {
		t.Fatalf("identity = %+v, exists = %v", identity, exists)
	}
	if identity.digest != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("root digest = %q", identity.digest)
	}
	if identity.platform != "linux/arm64/v8" || identity.variantDigest != "sha256:2222222222222222222222222222222222222222222222222222222222222222" {
		t.Fatalf("variant identity = %+v", identity)
	}
}

func TestIssue84AppleMissingVariantIsNotLocal(t *testing.T) {
	data := []byte(`[{"descriptor":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"variants":[{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "linux/amd64")
	if !exists || !identity.notLocal {
		t.Fatalf("identity = %+v, exists = %v", identity, exists)
	}
	if (appleEngine{}).parseImageExists(data, "linux/amd64") {
		t.Fatal("missing platform variant reported as locally present")
	}
}

func TestIssue84DockerBareImageIDCanonicalizesAfterInspect(t *testing.T) {
	id := strings.Repeat("a", 64)
	data := []byte(fmt.Sprintf(`[{"Id":"sha256:%s"}]`, id))
	identity, exists := (dockerEngine{}).parseImageIdentity(data, id, "")
	if !exists || !identity.pinned || identity.reference != "sha256:"+id || identity.id != "sha256:"+id {
		t.Fatalf("identity = %+v, exists = %v", identity, exists)
	}
}

type issue84PlatformRunner struct {
	calls int
}

func (r *issue84PlatformRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls++
	return []byte(fmt.Sprintf(`[{"Id":"sha256:%s","Os":"linux","Architecture":"amd64"}]`, strings.Repeat("a", 64))), nil, nil
}

func TestIssue84PlatformResolutionOnlyMatchesOSOnly(t *testing.T) {
	runner := &issue84PlatformRunner{}
	info := &engineInfo{platform: "linux/arm64"}
	platform, err := resolveContainerPlatform(context.Background(), dockerEngine{}, runner, info, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if platform != "linux/arm64" || runner.calls != 0 {
		t.Fatalf("platform = %q, calls = %d; OS mismatch must not resolve", platform, runner.calls)
	}
	if err := checkPlatformCompatibility("linux/amd64", platform); err == nil {
		t.Fatal("OS mismatch was accepted")
	}

	info = &engineInfo{platform: "linux", imageID: "sha256:" + strings.Repeat("a", 64)}
	platform, err = resolveContainerPlatform(context.Background(), dockerEngine{}, runner, info, "linux/amd64")
	if err != nil || platform != "linux/amd64" || runner.calls != 1 {
		t.Fatalf("matching OS-only platform = %q, calls = %d, err = %v", platform, runner.calls, err)
	}
}

func TestIssue84ExplicitPublishedAddressFailsClosed(t *testing.T) {
	p := publishSpec{containerPort: 80, proto: "tcp", hostAddr: "127.0.0.1", hostPort: 18080}
	if hasPublishedBinding([]boundPort{{containerPort: 80, proto: "tcp", hostPort: 18080}}, p) {
		t.Fatal("missing observed host address was accepted")
	}
	if !hasPublishedBinding([]boundPort{{containerPort: 80, proto: "tcp", hostAddr: "127.0.0.1", hostPort: 18080}}, p) {
		t.Fatal("matching observed host address was rejected")
	}
	p.hostAddr = "::1"
	if !hasPublishedBinding([]boundPort{{containerPort: 80, proto: "tcp", hostAddr: "0:0:0:0:0:0:0:1", hostPort: 18080}}, p) {
		t.Fatal("equivalent IPv6 host address was rejected")
	}
}

func TestIssue84DockerCreationMetadataIsAnIdentityWitness(t *testing.T) {
	before := &engineInfo{
		uid:     strings.Repeat("a", 64),
		created: "2026-01-01T00:00:00Z",
		labels:  map[string]string{managedLabel: "true", reuseLabel: "true", creationLabel: "aaaaaaaaaaaaaaaa"},
		imageID: "sha256:" + strings.Repeat("b", 64),
	}
	after := *before
	if !sameEngineIdentity(dockerEngine{}, before, &after) {
		t.Fatal("matching Docker identity was rejected")
	}
	after.created = "2026-01-01T00:00:01Z"
	if sameEngineIdentity(dockerEngine{}, before, &after) {
		t.Fatal("changed Docker creation metadata was accepted")
	}
	before.created = ""
	if err := validateReuseIdentity(dockerEngine{}, before); err == nil {
		t.Fatal("missing Docker creation metadata was accepted")
	}
}
