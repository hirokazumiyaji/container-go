package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// A cancelled child is signalled, so its -1 exit code is not an app result.
// IsCommandExit must veto it, otherwise Exec reports "exit -1, no error" and
// a caller cancellation silently becomes a success.
func TestReviewIsCommandExitVetoesContextError(t *testing.T) {
	cliErr := &cli.CLIError{Binary: "docker", Args: []string{"exec", "x"}, ExitCode: -1}

	if !cli.IsCommandExit(cliErr) {
		t.Fatal("plain CLIError should be a command exit")
	}
	if cli.IsCommandExit(nil) {
		t.Error("nil must not be a command exit")
	}
	if cli.IsCommandExit(errors.Join(cliErr, context.DeadlineExceeded)) {
		t.Error("CLIError joined with DeadlineExceeded must not be a command exit")
	}
	if cli.IsCommandExit(errors.Join(cliErr, context.Canceled)) {
		t.Error("CLIError joined with Canceled must not be a command exit")
	}
}

// A matched-but-malformed inspect record must not be reported as
// ErrContainerNotFound: callers treat that as already gone and would leak a
// live container.
func TestReviewDockerInspectMalformedRecordIsNotNotFound(t *testing.T) {
	id := strings.Repeat("a", 64)
	eng := dockerEngine{}

	// Matches by ID but carries no State.
	malformed := `[{"Id":"` + id + `","Name":"/x","Config":{"Labels":{}}}]`
	_, err := eng.parseInspect([]byte(malformed), id)
	if err == nil {
		t.Fatal("malformed record unexpectedly parsed")
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("malformed record reported as not-found (would leak a live container): %v", err)
	}
	if !strings.Contains(err.Error(), "no container-shaped state") {
		t.Errorf("unexpected error: %v", err)
	}

	wellFormed := `[{"Id":"` + id + `","Name":"/x","State":{"Status":"running"},"Config":{"Labels":{}}}]`
	if _, err := eng.parseInspect([]byte(wellFormed), id); err != nil {
		t.Fatalf("well-formed record failed to parse: %v", err)
	}

	absent := `[{"Id":"` + strings.Repeat("b", 64) + `","Name":"/y","State":{"Status":"running"},"Config":{"Labels":{}}}]`
	if _, err := eng.parseInspect([]byte(absent), id); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("absent target should be not-found, got %v", err)
	}
}

// PruneReuseGroup force-removes every container tagged with the group, so a
// group member without the managed label must still be deleted.
func TestReviewPruneReuseGroupIgnoresManagedLabel(t *testing.T) {
	creation := strings.Repeat("c", 16)
	candidate := pruneCandidate{
		id:         "ctr-a",
		creation:   creation,
		state:      StateRunning,
		managed:    false,
		reuseGroup: "grp",
	}
	fresh := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{creationLabel: creation, reuseGroupLabel: "grp"},
	}
	if !pruneCandidateStillCurrent(candidate, fresh, "grp") {
		t.Error("reuse-group member without managed label was skipped")
	}

	// Ordinary Prune still requires the managed label and a stopped state.
	if pruneCandidateStillCurrent(candidate, fresh, "") {
		t.Error("ordinary prune accepted an unmanaged candidate")
	}
	managedRunning := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", creationLabel: creation, reuseGroupLabel: "grp"},
	}
	if pruneCandidateStillCurrent(candidate, managedRunning, "") {
		t.Error("ordinary prune accepted a running container")
	}
}

// docker cp accepts created and stopped containers, so copy must not require
// a running state on that backend.
func TestReviewCopyRequiresRunningIsBackendSpecific(t *testing.T) {
	if !copyRequiresRunning(appleEngine{}) {
		t.Error("apple `container cp` requires a running container")
	}
	if copyRequiresRunning(dockerEngine{}) {
		t.Error("`docker cp` works on created and stopped containers")
	}
}

// A stream must not hold the non-reentrant name lock for its lifetime,
// otherwise two concurrent log readers serialize behind the first.
func TestReviewVerifiedReadTargetReleasesNameLock(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())

	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := ctr.verifiedReadTarget(ctx)
		cancel()
		if err != nil {
			t.Fatalf("verifiedReadTarget call %d: %v", i, err)
		}
	}
}
