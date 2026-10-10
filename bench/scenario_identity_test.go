//go:build integration

package bench

import (
	"testing"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
)

// ryukIdentityBackend answers reaper inspection from fixed content so the
// identity assertion can be exercised without a Docker daemon.
func ryukIdentityBackend(sessionID, reference, imageID, digest string) ibench.Backend {
	return ibench.Backend{
		ContainerInspect: func(name string) (ibench.ContainerIdentity, error) {
			return ibench.ContainerIdentity{
				ID:             "container-id",
				Name:           name,
				ImageReference: reference,
				ImageID:        imageID,
				Labels: map[string]string{
					"org.testcontainers.sessionId": sessionID,
					"org.testcontainers.reaper":    "true",
					"org.testcontainers.ryuk":      "true",
				},
			}, nil
		},
		ImageID: func(string) (string, error) { return imageID, nil },
		ImageDigest: func(string) (string, error) {
			return digest, nil
		},
	}
}

func TestVerifyTestcontainersRyukUsesActualContainerIdentity(t *testing.T) {
	const sessionID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	backend := ryukIdentityBackend(sessionID, ibench.TestcontainersRyukTag, "sha256:content", ibench.TestcontainersRyukImageDigest)
	if err := verifyTestcontainersRyuk(backend, sessionID); err != nil {
		t.Fatalf("verified identity: %v", err)
	}
	// A mismatched image ID must fail closed even when the tag still has the
	// expected repository digest.
	backend.ImageID = func(string) (string, error) { return "sha256:other", nil }
	if err := verifyTestcontainersRyuk(backend, sessionID); err == nil {
		t.Fatal("mismatched reaper image ID was accepted")
	}
}

// The immutable pinned reference is the stronger of the two accepted
// references: it names the content, so it must satisfy the same ID and
// digest checks as the mutable tag.
func TestVerifyTestcontainersRyukAcceptsPinnedReference(t *testing.T) {
	const sessionID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	backend := ryukIdentityBackend(sessionID, ibench.PinnedTestcontainersRyukImage, "sha256:content", ibench.PinnedTestcontainersRyukImageDigest)
	if err := verifyTestcontainersRyuk(backend, sessionID); err != nil {
		t.Fatalf("pinned reference rejected: %v", err)
	}

	// The digest has to be the one the reference resolves to, not merely a
	// digest verified for some other image.
	backend.ImageDigest = func(string) (string, error) {
		return "sha256:0000000000000000000000000000000000000000000000000000000000000000", nil
	}
	if err := verifyTestcontainersRyuk(backend, sessionID); err == nil {
		t.Fatal("reaper image with an unpinned digest was accepted")
	}

	backend = ryukIdentityBackend(sessionID, "testcontainers/ryuk:0.13.0", "sha256:content", ibench.PinnedTestcontainersRyukImageDigest)
	if err := verifyTestcontainersRyuk(backend, sessionID); err == nil {
		t.Fatal("reaper image from an unpinned reference was accepted")
	}
	backend = ryukIdentityBackend(sessionID, "evil/ryuk:0.14.0", "sha256:content", ibench.PinnedTestcontainersRyukImageDigest)
	if err := verifyTestcontainersRyuk(backend, sessionID); err == nil {
		t.Fatal("reaper image from an unexpected repository was accepted")
	}
}
