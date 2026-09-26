package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A running same-name container owned by a different reuse group must be
// rejected before adoption. checkReuseLabels validates the reuse, managed and
// creation labels, but those cannot tell one group from another, so the
// running-adoption path was unguarded while the stopped path was not. A
// WithReuseGroup("A") caller could otherwise adopt group B's live container
// and later terminate it.
func TestReviewRunningAdoptionRejectsForeignReuseGroup(t *testing.T) {
	creation := strings.Repeat("c", 16)
	foreign := &engineInfo{
		state: StateRunning,
		image: "docker.io/library/redis:7-alpine",
		labels: map[string]string{
			reuseLabel:      "true",
			managedLabel:    "true",
			creationLabel:   creation,
			reuseGroupLabel: "group-b",
		},
	}
	cfg := &config{name: "myctr", reuseGroup: "group-a", eng: appleEngine{}}

	if err := checkReuseOwned(foreign, "redis:7-alpine", cfg); err != nil {
		t.Fatalf("checkReuseOwned: %v", err)
	}
	// checkReuseOwned cannot see the group difference; only checkReuseGroup can.
	if err := checkReuseGroup(foreign, cfg); err == nil {
		t.Error("a foreign reuse group was accepted for adoption")
	}
	// The matching group is accepted.
	foreign.labels[reuseGroupLabel] = "group-a"
	if err := checkReuseGroup(foreign, cfg); err != nil {
		t.Errorf("matching reuse group rejected: %v", err)
	}
}

// A replaced or already-absent generation is not a leak. Reporting it through
// CleanupError is a false alarm on the exact signal that sentinel exists for.
func TestReviewRollbackDoesNotReportFalseLeak(t *testing.T) {
	for _, sentinel := range []error{ErrGenerationReplaced, ErrContainerNotFound} {
		f := newTestRunner()
		ctr := runTestContainer(t, f)
		// Force Terminate to fail with the sentinel.
		ctr.runner = &sentinelTerminateRunner{fakeRunner: f, sentinel: sentinel}

		cause := errors.New("copy failed")
		err := ctr.rollback(context.Background(), cause)
		if !errors.Is(err, cause) {
			t.Errorf("sentinel %v: original cause lost: %v", sentinel, err)
		}
		if strings.Contains(err.Error(), "left behind") {
			t.Errorf("sentinel %v: reported a false leak: %v", sentinel, err)
		}
		var cleanupErr *CleanupError
		if errors.As(err, &cleanupErr) {
			t.Errorf("sentinel %v: unexpected CleanupError: %v", sentinel, err)
		}
	}
}

// A real deletion failure must still be reported, so the false-leak fix does
// not swallow genuine leaks.
func TestReviewRollbackStillReportsRealLeak(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &sentinelTerminateRunner{fakeRunner: f, sentinel: errors.New("daemon refused")}

	cause := errors.New("copy failed")
	err := ctr.rollback(context.Background(), cause)
	if err == nil {
		t.Fatal("rollback returned nil")
	}
	if !strings.Contains(err.Error(), "left behind") {
		t.Errorf("err = %v, want the leak reported", err)
	}
}

// The three name-lock barriers must be distinct. transitional comes from the
// user cache dir and state from the user state dir, so pointing both at the
// same root collapsed two barriers onto one path, which is a self-inflicted
// deadlock rather than a redundant lock: flock belongs to the open file
// description, so the second LOCK_EX returns EWOULDBLOCK and acquisition spins
// until the context expires.
func TestReviewNameLockPathsMustBeDistinct(t *testing.T) {
	if _, err := resolveNameLocks("myctr"); err != nil {
		t.Fatalf("normal configuration: %v", err)
	}

	// Collapse the transitional and state roots onto one directory.
	shared := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", shared)
	t.Setenv("XDG_STATE_HOME", shared)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := resolveNameLocks("myctr"); err == nil {
		t.Skip("platform roots did not collapse; the aliasing case is not reachable here")
	} else if !strings.Contains(err.Error(), "same path") {
		t.Errorf("err = %v, want a same-path diagnostic", err)
	}
}

// sentinelTerminateRunner fails every delete with a chosen error.
type sentinelTerminateRunner struct {
	*fakeRunner
	sentinel error
}

func (s *sentinelTerminateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && (args[0] == "rm" || args[0] == "delete") {
		return nil, nil, s.sentinel
	}
	return s.fakeRunner.Run(ctx, args...)
}
