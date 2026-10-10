package container

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/bench"
	"github.com/hirokazumiyaji/container-go/internal/integrationtest"
)

// selectionProbeEnv carries the selection into the child process. It is
// deliberately not CONTAINERGO_BACKEND: the real TestMain unsets that
// variable before any test runs, so a child could not read it back.
const selectionProbeEnv = "CONTAINERGO_SELECTION_PROBE"

// TestSelectedBackendSurvivesTestMainUnsetEnv is the regression. The root
// test binary's TestMain unsets CONTAINERGO_BACKEND before any test runs,
// so a guard that reads the environment directly always sees "" and can
// never skip. That is what made `make integration
// CONTAINERGO_BACKEND=docker` run the Apple scenarios too.
//
// The child's TestMain has already unset the variable by the time this
// runs, so the selection has to travel under a different name and the
// child has to replay TestMain's record-then-unset sequence itself.
func TestSelectedBackendSurvivesTestMainUnsetEnv(t *testing.T) {
	for _, selection := range []string{"apple", "docker"} {
		t.Run("selection="+selection, func(t *testing.T) {
			out := runSelectionProbe(t, selection)
			if !strings.Contains(out, "selected="+selection) {
				t.Errorf("SelectedBackend() did not report %q after the variable was unset:\n%s", selection, out)
			}
		})
	}
}

// TestSelectionDrivesThePreflightVerdicts checks the guards the selection
// exists to drive. With docker selected, the Apple suite must skip and
// the Docker suite must proceed.
func TestSelectionDrivesThePreflightVerdicts(t *testing.T) {
	out := runSelectionProbe(t, "docker")
	if !strings.Contains(out, "apple-verdict=skip") {
		t.Errorf("the apple suite did not skip with docker selected:\n%s", out)
	}
	if !strings.Contains(out, "docker-verdict=proceed") {
		t.Errorf("the docker suite did not proceed with docker selected:\n%s", out)
	}
}

// TestBenchAvailableHonorsTheRecordedSelection covers the second guard
// with the same defect: internal/bench's Available read the environment,
// so it could never skip either.
func TestBenchAvailableHonorsTheRecordedSelection(t *testing.T) {
	out := runSelectionProbe(t, "docker")
	if !strings.Contains(out, "bench-apple=skipped") {
		t.Errorf("bench Available skipped nothing for the apple backend with docker selected:\n%s", out)
	}
	if !strings.Contains(out, "bench-docker=available") {
		t.Errorf("bench Available skipped its own backend with docker selected:\n%s", out)
	}
}

// TestBenchAvailableFallsBackToTheEnvironment covers the other module.
// The bench module has no TestMain, so it never calls
// SetSelectedBackend; reading only the recorded value would leave its
// selection dead there.
func TestBenchAvailableFallsBackToTheEnvironment(t *testing.T) {
	restore := integrationtest.SetSelectedBackendForTest("")
	t.Cleanup(restore)
	t.Setenv("CONTAINERGO_BACKEND", "docker")

	if reason := bench.AppleBackend().SkipReason(); !strings.Contains(reason, "skipping apple") {
		t.Errorf("SkipReason for apple = %q, want it to skip", reason)
	}
	// The selected backend must not skip for the selection reason (it may
	// still skip for a missing CLI or service, which is environment
	// dependent).
	if reason := bench.DockerBackend().SkipReason(); strings.Contains(reason, "CONTAINERGO_BACKEND=") {
		t.Errorf("SkipReason for the selected backend = %q, want it not to skip on the selection", reason)
	}
}

// runSelectionProbe re-runs this test binary as a child carrying the
// selection, and returns its output.
func runSelectionProbe(t *testing.T, selection string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestSelectionProbeChild", "-test.v")
	cmd.Env = append(os.Environ(), selectionProbeEnv+"="+selection)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("selection probe: %v\n%s", err, out)
	}
	return string(out)
}

// TestSelectionProbeChild is the child half of the probe. It replays what
// the real TestMain does to the process, in the same order, and then
// reports what each guard sees.
func TestSelectionProbeChild(t *testing.T) {
	selection := os.Getenv(selectionProbeEnv)
	if selection == "" {
		t.Skip("child of the backend-selection probe")
	}
	// The real TestMain ran before this test and already unset
	// CONTAINERGO_BACKEND. Nothing below may read the environment.
	if got := os.Getenv("CONTAINERGO_BACKEND"); got != "" {
		t.Fatalf("CONTAINERGO_BACKEND = %q in the child; TestMain did not unset it", got)
	}
	// Replay TestMain's record-then-unset.
	integrationtest.SetSelectedBackend(selection)
	os.Unsetenv("CONTAINERGO_BACKEND")

	t.Logf("selected=%s", integrationtest.SelectedBackend())

	probe := func() error { return nil }
	appleVerdict, appleReason := integrationtest.Decide("apple", integrationtest.SelectedBackend(), false, probe)
	t.Logf("apple-verdict=%s reason=%s", appleVerdict, appleReason)
	dockerVerdict, dockerReason := integrationtest.Decide("docker", integrationtest.SelectedBackend(), false, probe)
	t.Logf("docker-verdict=%s reason=%s", dockerVerdict, dockerReason)

	if reason := bench.AppleBackend().SkipReason(); strings.Contains(reason, "skipping apple") {
		t.Log("bench-apple=skipped")
	} else {
		t.Log("bench-apple=available")
	}
	if reason := bench.DockerBackend().SkipReason(); strings.Contains(reason, "skipping docker") {
		t.Log("bench-docker=skipped")
	} else {
		t.Log("bench-docker=available")
	}
}
