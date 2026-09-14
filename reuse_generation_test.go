package container

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTerminateRefusesReplacedGeneration(t *testing.T) {
	ctr := &Container{id: "myctr", eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	// Handle knows creation A, but live container reports creation B.
	ctr.runner = &generationRunner{
		creation: "bbbbbbbbbbbbbbbb",
	}
	if err := ctr.Terminate(context.Background()); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("Terminate = %v, want replaced-generation refusal", err)
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
