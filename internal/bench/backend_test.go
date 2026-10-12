package bench

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// skipWithoutPOSIXFakeCLI skips tests that install #!/bin/sh shims on PATH.
// Windows cannot execute those scripts, so the real docker/container CLI
// would be invoked against a missing daemon.
func skipWithoutPOSIXFakeCLI(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI scripts require a POSIX shell")
	}
}

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

func TestAppleImageIdentityUsesConfigurationDescriptor(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "apple-image-inspect.json"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parseAppleImageIdentity(data, "redis:7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Digest != RedisImageDigest || identity.Reference != "public.ecr.aws/docker/library/redis:7-alpine" || identity.ContentID != "858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestAppleImageIdentityRejectsIncompleteConfiguration(t *testing.T) {
	for name, data := range map[string]string{
		"missing name":       `[{"configuration":{"descriptor":{"digest":"sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"}}}]`,
		"missing descriptor": `[{"configuration":{"name":"redis:7-alpine"}}]`,
		"invalid digest":     `[{"configuration":{"name":"redis:7-alpine","descriptor":{"digest":"sha256:bad"}}}]`,
	} {
		if _, err := parseAppleImageIdentity([]byte(data), "redis:7-alpine"); err == nil {
			t.Errorf("%s Apple image provenance was accepted", name)
		}
	}
}

func TestAppleImageIdentityRejectsLegacyNestedSchema(t *testing.T) {
	legacy := []byte(`[{"configuration":{"image":{"reference":"redis:7-alpine","descriptor":{"digest":"sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"}}}}]`)
	if _, err := parseAppleImageIdentity(legacy, "redis:7-alpine"); err == nil {
		t.Fatal("legacy nested Apple image schema was accepted")
	}
}

func TestImageIdentityRejectsAmbiguousInspectResults(t *testing.T) {
	if _, err := parseDockerImageIdentity([]byte(`[{"Id":"sha256:one"},{"Id":"sha256:two"}]`), "redis:7-alpine"); err == nil {
		t.Fatal("ambiguous Docker image identity was accepted")
	}
	if _, err := parseAppleImageIdentity([]byte(`[{"id":"one"},{"id":"two"}]`), "redis:7-alpine"); err == nil {
		t.Fatal("ambiguous Apple image identity was accepted")
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
	skipWithoutPOSIXFakeCLI(t)
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
	skipWithoutPOSIXFakeCLI(t)
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
	skipWithoutPOSIXFakeCLI(t)
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
	skipWithoutPOSIXFakeCLI(t)
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

func TestContainerNotFoundClassifierIsSpecific(t *testing.T) {
	if !isContainerNotFoundError(errors.New("Error: no such container: reaper_session")) {
		t.Fatal("container not-found error was not recognized")
	}
	if isContainerNotFoundError(errors.New("Error: no such object")) {
		t.Fatal("generic no-such-object error was treated as a missing container")
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

func TestAppleVersionsRecordOnlyContainerAPIServer(t *testing.T) {
	skipWithoutPOSIXFakeCLI(t)
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "--version") echo 1.3.0 ;;
  "system version --format json") echo '[{"appName":"container","version":"1.3.0"},{"appName":"container-apiserver","version":"apiserver-version"}]' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	versions, err := AppleBackend().Versions()
	if err != nil {
		t.Fatal(err)
	}
	if versions[AppleClientVersionKey] != "1.3.0" || versions[AppleServiceVersionKey] != "apiserver-version" {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestAppleOptionalServiceVersionOnlyIgnoresUnsupportedCommand(t *testing.T) {
	skipWithoutPOSIXFakeCLI(t)
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

func TestAppleServiceVersionParsesOnlyAPIServerComponent(t *testing.T) {
	version, err := parseAppleServiceVersion([]byte(`[{"appName":"container","version":"1.3.0"},{"appName":"container-apiserver","version":"container-apiserver version 1.3.0 (build: release, commit: abc)","buildType":"release","commit":"abc"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if version != "container-apiserver version 1.3.0 (build: release, commit: abc)" {
		t.Fatalf("version = %q", version)
	}
	for name, data := range map[string]string{
		"client only": `[{"appName":"container","version":"1.3.0"}]`,
		"malformed":   `not-json`,
		"null":        `null`,
		"ambiguous":   `[{"appName":"container-apiserver","version":"1"},{"appName":"container-apiserver","version":"2"}]`,
	} {
		if _, err := parseAppleServiceVersion([]byte(data)); err == nil {
			t.Errorf("%s Apple system version was accepted", name)
		}
	}
}

func TestDockerCaptureProvenanceFailsClosedWhenUnavailable(t *testing.T) {
	if _, err := (Backend{Name: "docker"}).CaptureProvenance(); err == nil {
		t.Fatal("Docker provenance was accepted without a capture operation")
	}
	if _, err := (Backend{Name: "apple"}).CaptureProvenance(); err != nil {
		t.Fatalf("Apple empty provenance should be valid: %v", err)
	}
}

func TestDockerProvenanceRejectsConnectionOverrides(t *testing.T) {
	t.Setenv("DOCKER_API_VERSION", "1.44")
	if err := rejectDockerConnectionOverrides(); err == nil {
		t.Fatal("Docker API version override was accepted")
	}
	t.Setenv("DOCKER_API_VERSION", "")
	t.Setenv("DOCKER_AUTH_CONFIG", `{"auths":{}}`)
	if err := rejectDockerConnectionOverrides(); err == nil {
		t.Fatal("Docker auth override was accepted")
	}
}

func TestDockerProvenanceCapturesEffectiveContextAndDaemon(t *testing.T) {
	skipWithoutPOSIXFakeCLI(t)
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "info --format {{json .}}") echo '{"ID":"daemon-id","OperatingSystem":"Docker Desktop","OSType":"linux","Architecture":"arm64"}' ;;
  "context show") echo 'default' ;;
  "context inspect default") echo '[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	provenance, err := DockerBackend().CaptureProvenance()
	if err != nil {
		t.Fatal(err)
	}
	if provenance.Endpoint != "unix:///var/run/docker.sock" || provenance.Context != "default" || provenance.DaemonID != "daemon-id" || provenance.DaemonOS != "linux" || provenance.DaemonArch != "arm64" {
		t.Fatalf("provenance = %+v", provenance)
	}
}

func TestDockerProvenanceRejectsEndpointContextMismatch(t *testing.T) {
	skipWithoutPOSIXFakeCLI(t)
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "info --format {{json .}}") echo '{"ID":"daemon-id","OSType":"linux","Architecture":"arm64"}' ;;
  "context show") echo 'default' ;;
  "context inspect default") echo '[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "tcp://remote.example:2375")
	if _, err := DockerBackend().CaptureProvenance(); err == nil {
		t.Fatal("Docker endpoint/context mismatch was accepted")
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
