package bench

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// Backend wraps the engine-specific operations the scenarios need
// outside the timed region: probing the service, and inspecting,
// pulling, and removing images.
type Backend struct {
	// Name is the backend identifier: "apple" or "docker".
	Name string
	// Bin is the CLI binary of the backend.
	Bin string
	// VersionArgs prints the CLI (or daemon) version.
	VersionArgs []string
	// Probe reports whether the backend's service answers.
	Probe func() error
	// ImageExists reports whether the image is in the local store.
	ImageExists func(image string) (bool, error)
	// RemoveImage drops the image from the local store.
	RemoveImage func(image string) error
	// PullImage fetches the image into the local store.
	PullImage func(image string) error
}

// DockerBackend returns the harness for the docker CLI.
func DockerBackend() Backend {
	run := func(args ...string) ([]byte, error) {
		out, err := exec.Command("docker", args...).Output()
		return out, err
	}
	return Backend{
		Name:        "docker",
		Bin:         "docker",
		VersionArgs: []string{"version", "--format", "{{.Server.Version}}"},
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
	}
}

// AppleBackend returns the harness for the Apple container CLI.
func AppleBackend() Backend {
	run := func(args ...string) ([]byte, error) {
		out, err := exec.Command("container", args...).Output()
		return out, err
	}
	return Backend{
		Name:        "apple",
		Bin:         "container",
		VersionArgs: []string{"--version"},
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
