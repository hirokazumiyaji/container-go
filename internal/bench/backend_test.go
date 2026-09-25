package bench

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerRepoDigestSelectsPinnedRepository(t *testing.T) {
	got, err := dockerRepoDigest("testcontainers/ryuk:0.14.0", []byte(`["testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0"]`))
	if err != nil {
		t.Fatalf("dockerRepoDigest: %v", err)
	}
	if got != TestcontainersRyukImageDigest {
		t.Fatalf("digest = %q, want %q", got, TestcontainersRyukImageDigest)
	}
	if _, err := dockerRepoDigest("redis:7-alpine", []byte(`["redis@sha256:bad"]`)); err == nil {
		t.Fatal("dockerRepoDigest accepted a malformed digest")
	}
}

func TestBackendVersionsKeepDockerClientAndServerSeparate(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "version --format {{.Client.Version}}") echo 29.8.0 ;;
  "version --format {{.Server.Version}}") echo 29.8.0-server ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	versions, err := (DockerBackend()).Versions()
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if versions[DockerClientVersionKey] != "29.8.0" || versions[DockerServerVersionKey] != "29.8.0-server" {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestImageRemovalAccepted(t *testing.T) {
	t.Run("tolerates remove error when image is already absent", func(t *testing.T) {
		err := imageRemovalAccepted(
			func(string) (bool, error) { return false, nil },
			"redis:7-alpine",
			errors.New("no such image"),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects remove error when image remains", func(t *testing.T) {
		err := imageRemovalAccepted(
			func(string) (bool, error) { return true, nil },
			"redis:7-alpine",
			errors.New("image is in use"),
		)
		if err == nil {
			t.Fatal("want error when image is still present")
		}
		if !strings.Contains(err.Error(), "image still present") {
			t.Fatalf("error = %v, want image still present", err)
		}
	})

	t.Run("rejects when existence check fails", func(t *testing.T) {
		err := imageRemovalAccepted(
			func(string) (bool, error) { return false, errors.New("inspect failed") },
			"redis:7-alpine",
			errors.New("remove failed"),
		)
		if err == nil {
			t.Fatal("want error when existence check fails")
		}
		if !strings.Contains(err.Error(), "could not verify absence") {
			t.Fatalf("error = %v, want could not verify absence", err)
		}
	})
}
