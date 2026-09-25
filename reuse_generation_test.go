package container

import (
	"context"
	"errors"
	"fmt"
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
		state: StateStopped,
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "aaaaaaaaaaaaaaaa",
		},
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
		state: StateStopped,
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "aaaaaaaaaaaaaaaa",
		},
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
	info := &engineInfo{
		state: StateStopped,
		uid:   strings.Repeat("0f", 32),
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "aaaaaaaaaaaaaaaa",
		},
	}
	r := &dockerGenerationRunner{creation: "aaaaaaaaaaaaaaaa", uid: strings.Repeat("0f", 32)}
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

type reuseGenerationTransitionRunner struct {
	*fakeRunner
	before string
	after  string
	calls  int
}

func (r *reuseGenerationTransitionRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		r.calls++
		call := r.calls
		r.mu.Unlock()
		if call == 1 {
			return []byte(r.before), nil, nil
		}
		return []byte(r.after), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReuseFinalVerificationRejectsGenerationReplacement(t *testing.T) {
	runner := &reuseGenerationTransitionRunner{
		fakeRunner: newTestRunner(),
		before:     reuseInspectJSONWithCreation("review", "running", "redis:7-alpine", "aaaaaaaaaaaaaaaa"),
		after:      reuseInspectJSONWithCreation("review", "running", "redis:7-alpine", "bbbbbbbbbbbbbbbb"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("review"), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("error = %v, want generation replacement", err)
	}
	if runner.calls != 2 {
		t.Fatalf("inspect calls = %d, want initial and final", runner.calls)
	}
}

func TestReuseFinalVerificationRejectsDockerGenerationChangeWithSameUID(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	inspect := func(creation string) string {
		return fmt.Sprintf(`[{"Id":%q,"Name":"/review","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{"%s":"true","%s":"true","%s":%q}},"NetworkSettings":{}}]`, uid, managedLabel, reuseLabel, creationLabel, creation)
	}
	runner := &reuseGenerationTransitionRunner{
		fakeRunner: newTestRunner(),
		before:     inspect("aaaaaaaaaaaaaaaa"),
		after:      inspect("bbbbbbbbbbbbbbbb"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("review"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("error = %v, want generation replacement", err)
	}
}

func TestDeleteStoppedReuseSkipsRunningReplacement(t *testing.T) {
	info := &engineInfo{
		state: StateStopped,
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: "aaaaaaaaaaaaaaaa",
		},
	}
	r := &generationStateRunner{state: "running", creation: "aaaaaaaaaaaaaaaa"}
	cfg := &config{runner: r, eng: appleEngine{}, name: "shared"}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse = %v", err)
	}
	if r.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want 0 for running replacement", r.deleteCalls)
	}
}

func TestDockerInspectRejectsMissingOrMismatchedIdentity(t *testing.T) {
	cases := []string{
		`[{"Name":"/myctr","State":{"Status":"running"}}]`,
		`[{"Id":"not-a-container-id","Name":"/myctr","State":{"Status":"running"}}]`,
		`[{"Id":"` + strings.Repeat("a", 64) + `","Name":"/other","State":{"Status":"running"}}]`,
	}
	for _, data := range cases {
		if _, err := (dockerEngine{}).parseInspect([]byte(data), "myctr"); err == nil {
			t.Errorf("parseInspect(%s) = nil, want identity error", data)
		}
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
	if err == nil || !strings.Contains(err.Error(), "verify generation") {
		t.Fatalf("Terminate = %v, want verify-generation failure", err)
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
		return []byte(`[{"Id":"` + g.uid + `","Name":"/shared","State":{"Status":"exited"},"Config":{"Image":"redis","Labels":{"` + managedLabel + `":"true","` + reuseLabel + `":"true","` + creationLabel + `":"` + g.creation + `"}},"NetworkSettings":{}}]`), nil, nil
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
		labels := `,"labels":{"` + managedLabel + `":"true","` + reuseLabel + `":"true"`
		if g.creation != "" {
			labels += `,"` + creationLabel + `":"` + g.creation + `"`
		}
		labels += `}`
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
