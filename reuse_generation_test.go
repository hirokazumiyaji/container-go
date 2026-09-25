package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestTerminateRefusesReplacedGeneration(t *testing.T) {
	ctr := &Container{id: "myctr", eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	// Handle knows creation A, but live container reports creation B.
	ctr.runner = &generationRunner{
		creation: "bbbbbbbbbbbbbbbb",
	}
	if err := ctr.Terminate(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Terminate = %v, want ErrGenerationReplaced", err)
	}
}

func TestTerminateRejectsMissingOrInvalidGeneration(t *testing.T) {
	for _, generation := range []string{"", "not-a-generation"} {
		t.Run(generation, func(t *testing.T) {
			r := &generationRunner{creation: generation}
			ctr := &Container{id: "myctr", runner: r, eng: appleEngine{}, creation: generation}
			if err := ctr.Terminate(context.Background()); !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("Terminate = %v, want ErrGenerationReplaced", err)
			}
			if r.deleteCalls != 0 {
				t.Fatalf("deleteCalls = %d, want 0 for an unverifiable generation", r.deleteCalls)
			}
		})
	}
}

func TestErrGenerationReplacedDescribesOriginalContainer(t *testing.T) {
	got := ErrGenerationReplaced.Error()
	if !strings.Contains(got, "original verified container") {
		t.Fatalf("ErrGenerationReplaced = %q, want original-container wording", got)
	}
	if strings.Contains(strings.ToLower(got), "delete") {
		t.Fatalf("ErrGenerationReplaced = %q, want operation-level wording", got)
	}
}

type generationRunner struct {
	creation    string
	deleteCalls int
}

func (g *generationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return []byte(`[{"id":"myctr","configuration":{"id":"myctr","image":{"reference":"redis"},"labels":{"` + creationLabel + `":"` + g.creation + `"}},"status":{"state":"running","networks":[]}}]`), nil, nil
	case "system":
		return []byte("running"), nil, nil
	case "version":
		return []byte("ok"), nil, nil
	case "delete", "rm":
		g.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestTerminateAllowsMatchingGeneration(t *testing.T) {
	ctr := &Container{id: "myctr", eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	ctr.runner = &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate = %v, want nil for matching generation", err)
	}
}

func TestDeleteStoppedReuseSkipsMismatchedGeneration(t *testing.T) {
	cfg := &config{runner: &generationRunner{creation: "bbbbbbbbbbbbbbbb"}, eng: appleEngine{}, name: "shared"}
	info := &engineInfo{
		state:  StateStopped,
		labels: map[string]string{creationLabel: "aaaaaaaaaaaaaaaa"},
	}
	// Fresh inspect reports a different generation in running state, so
	// there is nothing stopped to delete.
	r := &generationStateRunner{creation: "bbbbbbbbbbbbbbbb", state: "running"}
	cfg.runner = r
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse = %v", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 for replaced generation", r.deleteCalls)
	}
}

func TestDeleteStoppedReuseSkipsUnlabeledReplacement(t *testing.T) {
	info := &engineInfo{
		state:  StateStopped,
		labels: map[string]string{creationLabel: "aaaaaaaaaaaaaaaa"},
	}
	r := &generationStateRunner{creation: "", state: "stopped"}
	cfg := &config{runner: r, eng: appleEngine{}, name: "shared"}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse = %v", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 for unlabeled replacement", r.deleteCalls)
	}
}

func TestDeleteStoppedReuseDeletesByImmutableID(t *testing.T) {
	uid := strings.Repeat("0f", 32)
	info := &engineInfo{
		state:       StateStopped,
		uid:         uid,
		uidVerified: true,
		labels:      map[string]string{creationLabel: "aaaaaaaaaaaaaaaa"},
	}
	r := &dockerGenerationRunner{creation: "aaaaaaaaaaaaaaaa", uid: uid}
	cfg := &config{runner: r, eng: dockerEngine{}, name: "shared"}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse = %v", err)
	}
	// The delete must target the ID inspect returned, never the name,
	// so a same-name replacement created after the check is not found.
	if len(r.deleted) != 1 || r.deleted[0] != r.uid {
		t.Errorf("deleted = %v, want [%s]", r.deleted, r.uid)
	}
}

func TestTerminateSucceedsWithoutDeleteWhenContainerIsGone(t *testing.T) {
	r := &inspectErrorRunner{stderr: `inspect failed: not found: "myctr"`}
	ctr := &Container{id: "myctr", runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate = %v, want nil for a missing container", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 when nothing exists to delete", r.deleteCalls)
	}
}

func TestTerminateFailsClosedWhenInspectFails(t *testing.T) {
	r := &inspectErrorRunner{stderr: "daemon unavailable"}
	ctr := &Container{id: "myctr", runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	err := ctr.Terminate(context.Background())
	if !errors.Is(err, ErrGenerationReplaced) || !strings.Contains(err.Error(), "verify generation") {
		t.Fatalf("Terminate = %v, want ErrGenerationReplaced and verify-generation failure", err)
	}
	// Without a verified generation a name-based delete could hit a
	// same-name replacement, so none may be issued.
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 when the generation cannot be verified", r.deleteCalls)
	}
}

type inspectErrorRunner struct {
	stderr      string
	deleteCalls int
}

func (g *inspectErrorRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: g.stderr}
	case "system":
		return []byte("running"), nil, nil
	case "version":
		return []byte("ok"), nil, nil
	case "delete", "rm":
		g.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

type dockerGenerationRunner struct {
	creation string
	uid      string
	deleted  []string
}

func (g *dockerGenerationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return []byte(`[{"Id":"` + g.uid + `","Name":"/shared","State":{"Status":"exited"},"Config":{"Image":"redis","Labels":{"` + creationLabel + `":"` + g.creation + `"}},"NetworkSettings":{}}]`), nil, nil
	case "info":
		return []byte("ok"), nil, nil
	case "rm":
		g.deleted = append(g.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

type generationStateRunner struct {
	creation    string
	state       string
	deleteCalls int
}

func (g *generationStateRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		labels := ""
		if g.creation != "" {
			labels = `,"labels":{"` + creationLabel + `":"` + g.creation + `"}`
		}
		return []byte(`[{"id":"shared","configuration":{"id":"shared","image":{"reference":"redis"}` + labels + `},"status":{"state":"` + g.state + `","networks":[]}}]`), nil, nil
	case "system":
		return []byte("running"), nil, nil
	case "version":
		return []byte("ok"), nil, nil
	case "delete", "rm":
		g.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestReuseCreateHasIndependentPullBudget(t *testing.T) {
	oldAttach := reuseAttachTimeout
	oldPoll := reusePollInterval
	reuseAttachTimeout = 150 * time.Millisecond
	reusePollInterval = 10 * time.Millisecond
	defer func() { reuseAttachTimeout, reusePollInterval = oldAttach, oldPoll }()

	// Pull takes longer than the attach timeout; the leader's own
	// create must still succeed with its runTimeout budget.
	r := &slowPullRunner{pullDelay: 400 * time.Millisecond}
	cfg := &config{
		runner: r, eng: appleEngine{}, name: "shared-reuse",
		pullPolicy: PullAlways,
	}
	ctx := context.Background()
	ctr, err := reuseCreate(ctx, "redis:7-alpine", cfg)
	if err != nil {
		t.Fatalf("reuseCreate = %v, want success despite slow pull", err)
	}
	if ctr == nil || ctr.creation == "" {
		t.Fatal("reuseCreate did not assign a creation generation")
	}
}

type slowPullRunner struct {
	*fakeRunner
	pullDelay time.Duration
}

func (s *slowPullRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if s.fakeRunner == nil {
		s.fakeRunner = newTestRunner()
	}
	if (args[0] == "image" && len(args) > 1 && args[1] == "pull") || args[0] == "pull" {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(s.pullDelay):
		}
	}
	return s.fakeRunner.Run(ctx, args...)
}
