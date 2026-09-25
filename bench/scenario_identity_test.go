//go:build integration

package bench

import (
	"testing"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
)

func TestVerifyTestcontainersRyukUsesActualContainerIdentity(t *testing.T) {
	const sessionID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	backend := ibench.Backend{
		ContainerInspect: func(name string) (ibench.ContainerIdentity, error) {
			return ibench.ContainerIdentity{
				ID:             "container-id",
				Name:           name,
				ImageReference: ibench.TestcontainersRyukTag,
				ImageID:        "sha256:content",
				Labels: map[string]string{
					"org.testcontainers.sessionId": sessionID,
					"org.testcontainers.reaper":    "true",
					"org.testcontainers.ryuk":      "true",
				},
			}, nil
		},
		ImageID: func(string) (string, error) { return "sha256:content", nil },
		ImageDigest: func(string) (string, error) {
			return ibench.TestcontainersRyukImageDigest, nil
		},
	}
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
