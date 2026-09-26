// Package integrationtest provides the shared backend preflight for the
// integration suites. The suites live in two packages (container and
// container_test), so the helpers live here rather than in either test file.
package integrationtest

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// RequireBackendEnv makes a missing backend an error instead of a skip. Set
// it to 1 in CI so a backend outage, a renamed CLI, or a missing Apple
// service fails the required job. Without it, a job whose backend is
// unavailable passes with zero tests run.
const RequireBackendEnv = "REQUIRE_BACKEND"

// selected records the process-level CONTAINERGO_BACKEND value.
//
// The test binary's TestMain unsets the variable so a developer's shell
// cannot redirect fixture-backed tests to another backend. The value is
// recorded here first, so a preflight can still honor an explicit
// selection and, more importantly, can still reject a typo.
var selected string

// SetSelectedBackend records the CONTAINERGO_BACKEND value the process was
// started with. Call it before unsetting the variable.
func SetSelectedBackend(value string) { selected = value }

// SelectedBackend returns the recorded process-level selection.
func SelectedBackend() string { return selected }

// Preflight resolves one backend for an integration suite.
//
// A CONTAINERGO_BACKEND value outside {"", "apple", "docker"} is always an
// error: it is a typo, and skipping on it hides the mistake behind a green
// job. Selecting the other backend is a skip, because the suite legitimately
// runs once per backend.
//
// unavailable reports why the backend cannot run. A missing CLI, a stopped
// daemon, or an absent Apple service skips locally and fails under
// RequireBackendEnv.
func Preflight(t *testing.T, want string, unavailable func() error) {
	t.Helper()
	verdict, reason := Decide(want, SelectedBackend(), Required(), unavailable)
	switch verdict {
	case Skip:
		t.Skip(reason)
	case Fail:
		t.Fatal(reason)
	}
	t.Setenv("CONTAINERGO_BACKEND", want)
}

// Verdict is the outcome of a backend preflight.
type Verdict int

const (
	// Proceed means the backend is usable.
	Proceed Verdict = iota
	// Skip means the suite does not apply to this run.
	Skip
	// Fail means the run must not report success.
	Fail
)

// Decide reports whether a suite should proceed, skip, or fail.
//
// selected is the CONTAINERGO_BACKEND value and required reports whether
// RequireBackendEnv is set. A typo is always a failure, because skipping on
// one hides the mistake behind a green job.
//
// required means this run must exercise want. Two things would otherwise
// leave a required job green with nothing executed: a backend that cannot
// run, and a selection naming the other backend. Both fail under required.
// Without it, both skip, so a developer without the backend can still run
// the suite locally.
func Decide(want, selected string, required bool, unavailable func() error) (Verdict, string) {
	switch selected {
	case "", want:
	default:
		if selected != "apple" && selected != "docker" {
			return Fail, fmt.Sprintf("invalid CONTAINERGO_BACKEND=%q: valid values are \"apple\" and \"docker\"", selected)
		}
		if required {
			return Fail, fmt.Sprintf("%s integration is required but CONTAINERGO_BACKEND=%s selects the other backend",
				want, selected)
		}
		return Skip, fmt.Sprintf("CONTAINERGO_BACKEND=%s; skipping %s integration", selected, want)
	}

	if err := unavailable(); err != nil {
		if required {
			return Fail, fmt.Sprintf("%s integration is required but unavailable: %v", want, err)
		}
		return Skip, fmt.Sprintf("%s integration unavailable: %v", want, err)
	}
	return Proceed, ""
}

// Required reports whether a missing backend must fail the run.
func Required() bool {
	return os.Getenv(RequireBackendEnv) == "1"
}

// DockerUnavailable reports why the Docker backend cannot run, or nil.
func DockerUnavailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI: %w", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return fmt.Errorf("docker daemon: %w", err)
	}
	return nil
}

// AppleUnavailable reports why the Apple backend cannot run, or nil.
func AppleUnavailable() error {
	if _, err := exec.LookPath("container"); err != nil {
		return fmt.Errorf("container CLI: %w", err)
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		return fmt.Errorf("apple container system service: %w", err)
	}
	return nil
}
