package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Stable keys used in Env.CLIs. Docker client and server versions are
// intentionally separate values.
const (
	DockerClientVersionKey = "docker.client"
	DockerServerVersionKey = "docker.server"
	AppleClientVersionKey  = "apple.client"
	AppleServiceVersionKey = "apple.service"
)

// Backend wraps the engine-specific operations the scenarios need
// outside the timed region: probing the service, and inspecting,
// pulling, and removing images.
type Backend struct {
	// Name is the backend identifier: "apple" or "docker".
	Name string
	// Bin is the CLI binary of the backend.
	Bin string
	// VersionArgs prints the backend client version.
	VersionArgs []string
	// ServerVersionArgs prints the Docker server version. It is empty for
	// backends that do not expose an independent server version.
	ServerVersionArgs []string
	// ServiceVersionArgs prints an optional backend service version.
	ServiceVersionArgs []string
	// ServiceVersionOptional permits older Apple CLIs that predate
	// "container system version".
	ServiceVersionOptional bool
	// Probe reports whether the backend's service answers.
	Probe func() error
	// ImageExists reports whether the image is in the local store.
	ImageExists func(image string) (bool, error)
	// RemoveImage drops the image from the local store.
	RemoveImage func(image string) error
	// PullImage fetches the image into the local store.
	PullImage func(image string) error
	// ImageDigest returns the repository digest currently resolved for an
	// image. It is used to verify locally cached mutable tags.
	ImageDigest func(image string) (string, error)
	// TagImage points target at the already-present source image.
	TagImage func(source, target string) error
}

// DockerBackend returns the harness for the docker CLI.
func DockerBackend() Backend {
	run := func(args ...string) ([]byte, error) {
		out, err := exec.Command("docker", args...).Output()
		return out, err
	}
	return Backend{
		Name:              "docker",
		Bin:               "docker",
		VersionArgs:       []string{"version", "--format", "{{.Client.Version}}"},
		ServerVersionArgs: []string{"version", "--format", "{{.Server.Version}}"},
		Probe: func() error {
			_, err := run("version")
			return err
		},
		ImageExists: func(image string) (bool, error) {
			_, err := run("image", "inspect", image)
			return err == nil, nil //nolint:nilerr // inspect fails when the image is absent
		},
		RemoveImage: func(image string) error {
			_, err := run("rmi", "-f", image)
			return err
		},
		PullImage: func(image string) error {
			_, err := run("pull", "--quiet", image)
			return err
		},
		ImageDigest: func(image string) (string, error) {
			out, err := run("image", "inspect", image, "--format", "{{json .RepoDigests}}")
			if err != nil {
				return "", err
			}
			return dockerRepoDigest(image, out)
		},
		TagImage: func(source, target string) error {
			_, err := run("tag", source, target)
			return err
		},
	}
}

// AppleBackend returns the harness for the Apple container CLI.
func AppleBackend() Backend {
	run := func(args ...string) ([]byte, error) {
		out, err := exec.Command("container", args...).Output()
		return out, err
	}
	return Backend{
		Name:                   "apple",
		Bin:                    "container",
		VersionArgs:            []string{"--version"},
		ServiceVersionArgs:     []string{"system", "version", "--format", "json"},
		ServiceVersionOptional: true,
		Probe: func() error {
			_, err := run("system", "status")
			return err
		},
		ImageExists: func(image string) (bool, error) {
			_, err := run("image", "inspect", image)
			return err == nil, nil //nolint:nilerr // inspect fails when the image is absent
		},
		RemoveImage: func(image string) error {
			_, err := run("image", "delete", image)
			return err
		},
		PullImage: func(image string) error {
			_, err := run("image", "pull", image)
			return err
		},
	}
}

// Versions records independently identifiable client and service/daemon
// versions under stable Env.CLIs keys.
func (b Backend) Versions() (map[string]string, error) {
	versions := make(map[string]string)
	run := func(args ...string) (string, error) {
		out, err := exec.Command(b.Bin, args...).Output()
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(string(out))
		if value == "" {
			return "", fmt.Errorf("%s %s returned an empty version", b.Bin, strings.Join(args, " "))
		}
		return value, nil
	}

	switch b.Name {
	case "docker":
		client, err := run(b.VersionArgs...)
		if err != nil {
			return nil, fmt.Errorf("record Docker client version: %w", err)
		}
		server, err := run(b.ServerVersionArgs...)
		if err != nil {
			return nil, fmt.Errorf("record Docker server version: %w", err)
		}
		versions[DockerClientVersionKey] = client
		versions[DockerServerVersionKey] = server
	case "apple":
		client, err := run(b.VersionArgs...)
		if err != nil {
			return nil, fmt.Errorf("record Apple client version: %w", err)
		}
		versions[AppleClientVersionKey] = client
		if len(b.ServiceVersionArgs) > 0 {
			service, err := run(b.ServiceVersionArgs...)
			if err == nil {
				versions[AppleServiceVersionKey] = service
			} else if !b.ServiceVersionOptional {
				return nil, fmt.Errorf("record Apple service version: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("record versions: unsupported backend %q", b.Name)
	}
	return versions, nil
}

func dockerRepoDigest(image string, output []byte) (string, error) {
	var references []string
	if err := json.Unmarshal(output, &references); err != nil {
		return "", fmt.Errorf("decode repository digests for %s: %w", image, err)
	}
	repository := image
	if before, _, ok := strings.Cut(repository, "@"); ok {
		repository = before
	} else if slash := strings.LastIndex(repository, "/"); slash >= 0 {
		if colon := strings.LastIndex(repository, ":"); colon > slash {
			repository = repository[:colon]
		}
	} else if colon := strings.LastIndex(repository, ":"); colon >= 0 {
		repository = repository[:colon]
	}
	prefix := repository + "@"
	for _, reference := range references {
		if strings.HasPrefix(reference, prefix) {
			digest := strings.TrimPrefix(reference, prefix)
			if validSHA256Digest(digest) {
				return digest, nil
			}
		}
	}
	return "", fmt.Errorf("image %s has no valid %s digest", image, repository)
}

// Available skips the test when the backend's CLI or service is
// missing, or when CONTAINERGO_BACKEND selects a different backend.
func (b Backend) Available(tb testing.TB) {
	tb.Helper()
	if want := os.Getenv("CONTAINERGO_BACKEND"); want != "" && want != b.Name {
		tb.Skipf("CONTAINERGO_BACKEND=%s; skipping %s", want, b.Name)
	}
	if _, err := exec.LookPath(b.Bin); err != nil {
		tb.Skipf("%s CLI not installed", b.Name)
	}
	if err := b.Probe(); err != nil {
		tb.Skipf("%s backend service not running: %v", b.Name, err)
	}
}

// EnsureImage guarantees the image is present before a timed
// iteration; preparation never lands in the measurement.
func (b Backend) EnsureImage(tb testing.TB, image string) {
	tb.Helper()
	ok, err := b.ImageExists(image)
	if err != nil {
		tb.Logf("image existence check for %s: %v", image, err)
	}
	if ok {
		return
	}
	if err := b.PullImage(image); err != nil {
		tb.Fatalf("pull %s: %v", image, err)
	}
}

// EnsureImageAbsent removes the image before a cold iteration.
// A remove that fails because the image is already gone is fine; any
// other failure where the image is still present aborts the scenario
// so a warm start is not recorded under a cold label.
func (b Backend) EnsureImageAbsent(tb testing.TB, image string) {
	tb.Helper()
	err := b.RemoveImage(image)
	if err == nil {
		return
	}
	if checkErr := imageRemovalAccepted(b.ImageExists, image, err); checkErr != nil {
		tb.Fatal(checkErr)
	}
	tb.Logf("remove %s reported error but image is absent: %v", image, err)
}

// imageRemovalAccepted reports whether a RemoveImage failure is
// tolerable because the image is already absent.
func imageRemovalAccepted(exists func(string) (bool, error), image string, removeErr error) error {
	ok, checkErr := exists(image)
	if checkErr != nil {
		return fmt.Errorf("remove %s: %w (and could not verify absence: %v)", image, removeErr, checkErr)
	}
	if ok {
		return fmt.Errorf("remove %s: %w (image still present)", image, removeErr)
	}
	return nil
}
