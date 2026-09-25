package bench

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerIdentityParsesContentAndContainerProvenance(t *testing.T) {
	identity, err := parseDockerImageIdentity([]byte(`[{"Id":"sha256:content","RepoTags":["redis:7-alpine","redis:stable"],"RepoDigests":["redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"]}]`), "redis:7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ContentID != "sha256:content" || identity.Digest != RedisImageDigest || len(identity.Tags) != 2 {
		t.Fatalf("identity = %+v", identity)
	}
	container, err := parseDockerContainerIdentity([]byte(`[{"Id":"abc","Name":"/reaper_session","Image":"sha256:content","Config":{"Image":"testcontainers/ryuk:0.14.0","Labels":{"org.testcontainers.reaper":"true"}}}]`), "reaper_session")
	if err != nil {
		t.Fatal(err)
	}
	if container.Name != "reaper_session" || container.ImageID != "sha256:content" || container.Labels["org.testcontainers.reaper"] != "true" {
		t.Fatalf("container identity = %+v", container)
	}
}

func TestAppleImageIdentityUsesNestedDescriptor(t *testing.T) {
	identity, err := parseAppleImageIdentity([]byte(`[{"configuration":{"image":{"reference":"docker.io/library/redis:7-alpine","descriptor":{"digest":"sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"}}}}]`), "redis:7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Digest != RedisImageDigest || identity.Reference != "docker.io/library/redis:7-alpine" {
		t.Fatalf("identity = %+v", identity)
	}
}

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

func TestDockerImageDigestReadsFullInspectOutput(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$*" = "image inspect redis:7-alpine" ]; then
  echo '[{"Id":"sha256:content","RepoTags":["redis:7-alpine"],"RepoDigests":["redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"]}]'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := DockerBackend().ImageDigest("redis:7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	if got != RedisImageDigest {
		t.Fatalf("digest = %q, want %q", got, RedisImageDigest)
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

func TestDockerImageExistsDistinguishesMissingFromOperationalFailure(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "image inspect redis:7-alpine")
    echo 'Error response from daemon: No such image: redis:7-alpine' >&2
    exit 1
    ;;
  "image inspect broken:inspect")
    echo 'Cannot connect to the Docker daemon' >&2
    exit 1
    ;;
  "image inspect malformed:inspect")
    echo 'not-json'
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	backend := DockerBackend()

	ok, err := backend.ImageExists("redis:7-alpine")
	if err != nil || ok {
		t.Fatalf("missing image = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = backend.ImageExists("broken:inspect")
	if err == nil || ok {
		t.Fatalf("operational image error = (%v, %v), want (false, error)", ok, err)
	}
	ok, err = backend.ImageExists("malformed:inspect")
	if err == nil || ok {
		t.Fatalf("malformed image output = (%v, %v), want (false, error)", ok, err)
	}
}

func TestAppleImageExistsDistinguishesMissingFromOperationalFailure(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "image inspect redis:7-alpine") echo 'image not found: redis:7-alpine' >&2; exit 1 ;;
  "image inspect broken:inspect") echo 'XPC connection unavailable' >&2; exit 1 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	backend := AppleBackend()
	ok, err := backend.ImageExists("redis:7-alpine")
	if err != nil || ok {
		t.Fatalf("missing Apple image = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = backend.ImageExists("broken:inspect")
	if err == nil || ok {
		t.Fatalf("operational Apple image error = (%v, %v), want (false, error)", ok, err)
	}
}

func TestVerifyImageAbsentChecksContentAndTags(t *testing.T) {
	identity := ImageIdentity{
		Reference: "redis:7-alpine",
		ContentID: "sha256:content",
		Digest:    RedisImageDigest,
		Tags:      []string{"redis:7-alpine", "redis:stable"},
	}
	present := map[string]bool{
		"redis:stable": true,
	}
	backend := Backend{
		Name: "fake",
		ImageExists: func(reference string) (bool, error) {
			return present[reference], nil
		},
	}
	if err := backend.verifyImageAbsent("redis:7-alpine", identity); err == nil {
		t.Fatal("verifyImageAbsent accepted a remaining tag")
	}
	delete(present, "redis:stable")
	if err := backend.verifyImageAbsent("redis:7-alpine", identity); err != nil {
		t.Fatalf("verifyImageAbsent: %v", err)
	}
	present[imageRepository(identity.Reference)+"@"+identity.Digest] = true
	if err := backend.verifyImageAbsent("redis:7-alpine", identity); err == nil {
		t.Fatal("verifyImageAbsent accepted remaining repository content")
	}
}

func TestBackendSelectionRejectsInvalidEnvironment(t *testing.T) {
	if err := validateBackendSelection("podman", "docker"); err == nil {
		t.Fatal("validateBackendSelection accepted an invalid backend")
	}
	if err := validateBackendSelection("apple", "docker"); err != nil {
		t.Fatalf("validateBackendSelection rejected a valid other backend: %v", err)
	}
}

func TestAppleOptionalServiceVersionOnlyIgnoresUnsupportedCommand(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "--version") echo 1.3.0 ;;
  "system version --format json") echo 'unknown command' >&2; exit 1 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	versions, err := AppleBackend().Versions()
	if err != nil {
		t.Fatalf("Versions with unsupported service command: %v", err)
	}
	if versions[AppleClientVersionKey] != "1.3.0" {
		t.Fatalf("versions = %+v", versions)
	}
	if _, ok := versions[AppleServiceVersionKey]; ok {
		t.Fatal("unsupported service command was recorded as a version")
	}

	operational := `#!/bin/sh
case "$*" in
  "--version") echo 1.3.0 ;;
  "system version --format json") echo 'service unavailable' >&2; exit 1 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(operational), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AppleBackend().Versions(); err == nil {
		t.Fatal("Versions ignored an operational Apple service error")
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
