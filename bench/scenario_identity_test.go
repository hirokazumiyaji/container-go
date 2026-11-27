//go:build integration

package bench

import (
	"testing"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
)

// ryukSessionID is the session the fixed backend reports. Every identity test
// uses it, so the helper reads it directly instead of taking it as a
// parameter that can only ever hold one value.
const ryukSessionID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// ryukImageID is the immutable image ID the fixed backend reports. The
// mutable tag and the pinned reference resolve to the same content, so it
// also carries the digest every caller expects.
const ryukImageID = "sha256:content"

// ryukImageDigest is the digest the fixed backend reports for ryukImageID.
const ryukImageDigest = ibench.TestcontainersRyukImageDigest

// ryukIdentityBackend answers reaper inspection from fixed content so the
// identity assertion can be exercised without a Docker daemon.
func ryukIdentityBackend(reference string) ibench.Backend {
	return ibench.Backend{
		ContainerInspect: func(name string) (ibench.ContainerIdentity, error) {
			return ibench.ContainerIdentity{
				ID:             "container-id",
				Name:           name,
				ImageReference: reference,
				ImageID:        ryukImageID,
				Labels: map[string]string{
					"org.testcontainers.sessionId": ryukSessionID,
					"org.testcontainers.reaper":    "true",
					"org.testcontainers.ryuk":      "true",
				},
			}, nil
		},
		ImageID: func(string) (string, error) { return ryukImageID, nil },
		ImageDigest: func(string) (string, error) {
			return ryukImageDigest, nil
		},
	}
}

func TestVerifyTestcontainersRyukUsesActualContainerIdentity(t *testing.T) {
	backend := ryukIdentityBackend(ibench.TestcontainersRyukTag)
	if err := verifyTestcontainersRyuk(backend, ryukSessionID); err != nil {
		t.Fatalf("verified identity: %v", err)
	}
	// A mismatched image ID must fail closed even when the tag still has the
	// expected repository digest.
	backend.ImageID = func(string) (string, error) { return "sha256:other", nil }
	if err := verifyTestcontainersRyuk(backend, ryukSessionID); err == nil {
		t.Fatal("mismatched reaper image ID was accepted")
	}
}

// The immutable pinned reference is the stronger of the two accepted
// references: it names the content, so it must satisfy the same ID and
// digest checks as the mutable tag.
func TestVerifyTestcontainersRyukAcceptsPinnedReference(t *testing.T) {
	backend := ryukIdentityBackend(ibench.PinnedTestcontainersRyukImage)
	if err := verifyTestcontainersRyuk(backend, ryukSessionID); err != nil {
		t.Fatalf("pinned reference rejected: %v", err)
	}

	// The digest has to be the one the reference resolves to, not merely a
	// digest verified for some other image.
	backend.ImageDigest = func(string) (string, error) {
		return "sha256:0000000000000000000000000000000000000000000000000000000000000000", nil
	}
	if err := verifyTestcontainersRyuk(backend, ryukSessionID); err == nil {
		t.Fatal("reaper image with an unpinned digest was accepted")
	}

	backend = ryukIdentityBackend("testcontainers/ryuk:0.13.0")
	if err := verifyTestcontainersRyuk(backend, ryukSessionID); err == nil {
		t.Fatal("reaper image from an unpinned reference was accepted")
	}
	backend = ryukIdentityBackend("evil/ryuk:0.14.0")
	if err := verifyTestcontainersRyuk(backend, ryukSessionID); err == nil {
		t.Fatal("reaper image from an unexpected repository was accepted")
	}
}
