package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

type issue83ReviewTransitionRunner struct {
	*fakeRunner
	before   string
	after    string
	afterNow bool

	mu          sync.Mutex
	inspectCall int
}

func (r *issue83ReviewTransitionRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.inspectCall++
	after := r.afterNow
	if r.inspectCall > 1 && r.after != "" {
		after = true
	}
	r.mu.Unlock()
	if after {
		return []byte(r.after), nil, nil
	}
	return []byte(r.before), nil, nil
}

func (r *issue83ReviewTransitionRunner) replace() {
	r.mu.Lock()
	r.afterNow = true
	r.mu.Unlock()
}

type issue83ReviewReplaceStrategy struct{ replace func() }

func (s issue83ReviewReplaceStrategy) WaitUntilReady(context.Context, wait.Target) error {
	s.replace()
	return nil
}

func issue83ReviewAppleInspect(name, creation, state string) string {
	return fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine"},"labels":{"%s":"true","%s":"true","%s":%q}},"status":{"state":%q,"networks":[]}}]`, name, name, managedLabel, reuseLabel, creationLabel, creation, state)
}

func TestIssue83ReuseRechecksGenerationAfterReadiness(t *testing.T) {
	runner := &issue83ReviewTransitionRunner{
		fakeRunner: newTestRunner(),
		before:     issue83ReviewAppleInspect("review", "aaaaaaaaaaaaaaaa", "running"),
		after:      issue83ReviewAppleInspect("review", "bbbbbbbbbbbbbbbb", "running"),
	}
	runner.imagePresent = true
	strategy := issue83ReviewReplaceStrategy{replace: runner.replace}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("review"), WithReuse(), withRunner(runner), withEngine(appleEngine{}),
		WithWaitStrategy(strategy))
	if err == nil || ctr != nil || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Run = (%v, %v), want generation replacement error", ctr, err)
	}
}

func TestIssue83ReuseFailsClosedWhenPostReadinessGenerationIsMissing(t *testing.T) {
	runner := &issue83ReviewTransitionRunner{
		fakeRunner: newTestRunner(),
		before:     issue83ReviewAppleInspect("review-missing", "aaaaaaaaaaaaaaaa", "running"),
		after:      issue83ReviewAppleInspect("review-missing", "", "running"),
	}
	runner.imagePresent = true
	strategy := issue83ReviewReplaceStrategy{replace: runner.replace}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("review-missing"), WithReuse(), withRunner(runner), withEngine(appleEngine{}),
		WithWaitStrategy(strategy))
	if err == nil || ctr != nil || !strings.Contains(err.Error(), "generation") || !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Run = (%v, %v), want missing-generation failure", ctr, err)
	}
}

func TestIssue83DeleteStoppedReuseDoesNotDeleteRunningReplacement(t *testing.T) {
	labels := map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: "aaaaaaaaaaaaaaaa",
	}
	r := &generationStateRunner{state: "running", creation: "aaaaaaaaaaaaaaaa"}
	cfg := &config{runner: r, eng: appleEngine{}, name: "shared"}
	if err := deleteStoppedReuse(context.Background(), cfg, &engineInfo{state: StateStopped, labels: labels}); err != nil {
		t.Fatalf("deleteStoppedReuse: %v", err)
	}
	if r.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want no delete for running replacement", r.deleteCalls)
	}
}

func TestIssue83TerminateRefusesZeroGenerationNameDelete(t *testing.T) {
	r := &issue83ReviewDeleteRunner{}
	ctr := &Container{id: "legacy", runner: r, eng: appleEngine{}}
	if err := ctr.Terminate(context.Background()); err == nil {
		t.Fatal("Terminate zero-generation name handle: want error")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no name delete", r.deleted)
	}
}

func TestIssue83TerminateAllowsLegacyHandleWithImmutableID(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r := &issue83ReviewDeleteRunner{}
	ctr := &Container{id: "legacy", runner: r, eng: dockerEngine{}, uid: uid}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate immutable legacy handle: %v", err)
	}
	if len(r.deleted) != 1 || r.deleted[0] != uid {
		t.Fatalf("deleted = %v, want [%s]", r.deleted, uid)
	}
}

type issue83ReviewDeleteRunner struct {
	deleted []string
}

func (r *issue83ReviewDeleteRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "delete" || args[0] == "rm" {
		r.deleted = append(r.deleted, args[len(args)-1])
	}
	return nil, nil, nil
}

func TestIssue83DockerInspectDoesNotFallbackFromIDToName(t *testing.T) {
	const target = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const other = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	data := []byte(`[{"Id":"` + other + `","Name":"/` + target + `","State":{"Status":"running"}}]`)
	if _, err := (dockerEngine{}).parseInspect(data, target); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("parseInspect error = %v, want target-not-found", err)
	}
}

func TestIssue83DockerInspectRejectsMissingAndMalformedID(t *testing.T) {
	cases := []string{
		`[{"Name":"/myctr","State":{"Status":"running"}}]`,
		`[{"Id":"not-a-container-id","Name":"/myctr","State":{"Status":"running"}}]`,
	}
	for _, data := range cases {
		if _, err := (dockerEngine{}).parseInspect([]byte(data), "myctr"); err == nil {
			t.Errorf("parseInspect(%s): want error", data)
		}
	}
}

func TestIssue83DockerTerminateDoesNotDeleteWithMalformedInspectID(t *testing.T) {
	for _, data := range []string{
		`[{"Name":"/myctr","State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{"` + creationLabel + `":"aaaaaaaaaaaaaaaa"}}}]`,
		`[{"Id":"not-a-container-id","Name":"/myctr","State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{"` + creationLabel + `":"aaaaaaaaaaaaaaaa"}}}]`,
	} {
		r := &issue83ReviewDockerInspectRunner{inspectJSON: data}
		ctr := &Container{id: "myctr", runner: r, eng: dockerEngine{}, creation: "aaaaaaaaaaaaaaaa"}
		if err := ctr.Terminate(context.Background()); err == nil {
			t.Fatal("Terminate: want invalid-identity error")
		}
		if len(r.deleted) != 0 {
			t.Fatalf("deleted = %v, want no delete", r.deleted)
		}
	}
}

func TestIssue83CleanupFailedCreateDoesNotFallbackToDockerName(t *testing.T) {
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	r := &issue83ReviewDockerInspectRunner{inspectJSON: `[{"Name":"myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{"` + managedLabel + `":"true","` + sessionLabel + `":"` + sessionID() + `","` + creationLabel + `":"aaaaaaaaaaaaaaaa"}}}]`}
	cfg := &config{runner: r, eng: dockerEngine{}, name: "myctr", creation: "aaaaaaaaaaaaaaaa"}
	cleanupFailedCreate(context.Background(), cfg, runErr, runErr)
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no name fallback without Docker ID", r.deleted)
	}
}

type issue83ReviewDockerInspectRunner struct {
	inspectJSON string
	deleted     []string
}

func (r *issue83ReviewDockerInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return []byte(r.inspectJSON), nil, nil
	case "rm", "delete":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestIssue83ReaperRejectsUnidentifiedName(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer r.closeStdin()
	if err := r.register("legacy", ""); err == nil {
		t.Fatal("reaper accepted a name with no generation")
	}
}
