package integrationtest

import (
	"errors"
	"strings"
	"testing"
)

// A CONTAINERGO_BACKEND typo must fail, never skip: skipping hides the
// mistake behind a green job.
func TestDecideRejectsBackendTypo(t *testing.T) {
	for _, selected := range []string{"Docker", "dockre", "appl", "podman", "1"} {
		verdict, reason := Decide("docker", selected, false, func() error { return nil })
		if verdict != Fail {
			t.Errorf("selected=%q verdict=%v, want Fail", selected, verdict)
		}
		if !strings.Contains(reason, "invalid CONTAINERGO_BACKEND") {
			t.Errorf("selected=%q reason=%q", selected, reason)
		}
	}
}

// A backend that cannot run must fail when the job requires it, so a backend
// outage or a renamed CLI is not reported as success.
func TestDecideRequiredBackendFailsWhenUnavailable(t *testing.T) {
	unavailable := func() error { return errors.New("docker daemon: exit 1") }

	verdict, reason := Decide("docker", "", true, unavailable)
	if verdict != Fail {
		t.Errorf("verdict=%v, want Fail when REQUIRE_BACKEND=1", verdict)
	}
	if !strings.Contains(reason, "required but unavailable") {
		t.Errorf("reason=%q", reason)
	}

	// The same failure skips locally, so a developer without the backend can
	// still run the default suite.
	if verdict, _ := Decide("docker", "", false, unavailable); verdict != Skip {
		t.Errorf("verdict=%v, want Skip without REQUIRE_BACKEND", verdict)
	}
}

// An available backend proceeds, and the other backend skips rather than
// fails: the suite legitimately runs once per backend.
func TestDecideProceedAndCrossBackendSkip(t *testing.T) {
	ok := func() error { return nil }
	if verdict, _ := Decide("docker", "", false, ok); verdict != Proceed {
		t.Errorf("verdict=%v, want Proceed", verdict)
	}
	if verdict, _ := Decide("docker", "docker", false, ok); verdict != Proceed {
		t.Errorf("verdict=%v, want Proceed for the selected backend", verdict)
	}
	verdict, reason := Decide("apple", "docker", false, ok)
	if verdict != Skip {
		t.Errorf("verdict=%v, want Skip for the other backend", verdict)
	}
	if !strings.Contains(reason, "skipping apple integration") {
		t.Errorf("reason=%q", reason)
	}
}

// A typo must fail even when the backend itself is unavailable, so the
// message names the typo rather than the outage.
func TestDecideTypoWinsOverUnavailable(t *testing.T) {
	unavailable := func() error { return errors.New("no backend") }
	verdict, reason := Decide("docker", "Docker", false, unavailable)
	if verdict != Fail {
		t.Fatalf("verdict=%v, want Fail", verdict)
	}
	if !strings.Contains(reason, "invalid CONTAINERGO_BACKEND") {
		t.Errorf("reason=%q", reason)
	}
}
