package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reuseCreateRunner answers inspect with not-found until a successful
// run, then serves a reused-container inspect payload.
type reuseCreateRunner struct {
	*fakeRunner
	created atomic.Bool
}

func newReuseCreateRunner() *reuseCreateRunner {
	f := newTestRunner()
	f.imagePresent = true
	return &reuseCreateRunner{fakeRunner: f}
}

func (r *reuseCreateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		created := r.created.Load()
		creation := r.creations[args[len(args)-1]]
		r.mu.Unlock()
		if !created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `inspect failed: not found: "myctr"`}
		}
		return []byte(reuseInspectJSONWithCreation(args[len(args)-1], "running", "redis:7-alpine", creation)), nil, nil
	}
	if args[0] == "run" {
		r.created.Store(true)
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestWithReuseRequiresName(t *testing.T) {
	_, err := Run(context.Background(), "redis:7-alpine", WithReuse(), withRunner(newTestRunner()))
	if err == nil || !strings.Contains(err.Error(), "WithReuse requires WithName") {
		t.Fatalf("error = %v, want WithReuse requires WithName", err)
	}
}

func TestWithReuseGroupRequiresReuse(t *testing.T) {
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuseGroup("integration"), withRunner(newTestRunner()))
	if err == nil || !strings.Contains(err.Error(), "WithReuseGroup requires WithReuse") {
		t.Fatalf("error = %v, want WithReuseGroup requires WithReuse", err)
	}
}

func TestReuseAddsLabels(t *testing.T) {
	f := newReuseCreateRunner()
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithReuseGroup("integration"),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join(f.callWith("run"), " ")
	if !strings.Contains(joined, "--label "+reuseLabel+"=true") {
		t.Errorf("missing reuse label: %s", joined)
	}
	if !strings.Contains(joined, "--label "+reuseGroupLabel+"=integration") {
		t.Errorf("missing reuse group label: %s", joined)
	}
	if !ctr.reused {
		t.Error("handle not marked reused")
	}
}

func TestTerminateContainerSkipsReused(t *testing.T) {
	f := newReuseCreateRunner()
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.calls = nil
	if err := TerminateContainer(ctr); err != nil {
		t.Fatalf("TerminateContainer: %v", err)
	}
	if f.callWith("delete") != nil {
		t.Error("TerminateContainer deleted a reused handle")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if f.callWith("delete") == nil {
		t.Error("explicit Terminate did not delete")
	}
}

func TestCleanupSkipsReused(t *testing.T) {
	f := newReuseCreateRunner()
	t.Run("inner", func(t *testing.T) {
		ctr, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithReuse(), withRunner(f), withEngine(appleEngine{}))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		Cleanup(t, ctr)
	})
	if f.callWith("delete") != nil {
		t.Error("Cleanup deleted a reused handle")
	}
}

func TestReuseCollapsesConcurrentCreates(t *testing.T) {
	f := newReuseCreateRunner()
	const n = 100
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]string, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(),
				withRunner(f), withEngine(appleEngine{}),
				WithWaitStrategy(&recordingStrategy{}),
			)
			errs[i] = err
			if ctr != nil {
				ids[i] = ctr.ID()
			}
		}(i)
	}
	wg.Wait()

	var runCalls int
	f.mu.Lock()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "run" {
			runCalls++
		}
	}
	f.mu.Unlock()
	if runCalls != 1 {
		t.Fatalf("run calls = %d, want 1", runCalls)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if ids[i] != "shared" {
			t.Fatalf("goroutine %d: id = %q", i, ids[i])
		}
	}
}

func reuseInspectJSON(id, state, image string) string {
	return reuseInspectJSONWithCreation(id, state, image, "aaaaaaaaaaaaaaaa")
}

func reuseInspectJSONWithCreation(id, state, image, creation string) string {
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": %q},
      "publishedPorts": [],
      "labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.reuse": "true",
        "com.github.hirokazumiyaji.container-go.creation": %q
      }
    },
    "status": {
      "state": %q,
      "networks": [{"ipv4Address": "192.168.64.3/24", "network": "default"}]
    }
  }
]`, id, id, image, creation, state)
}

type attachRunner struct {
	*fakeRunner
	state string
}

func (a *attachRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		a.mu.Lock()
		a.calls = append(a.calls, args)
		a.mu.Unlock()
		return []byte(reuseInspectJSON(args[len(args)-1], a.state, "redis:7-alpine")), nil, nil
	}
	if args[0] == "run" {
		return nil, nil, &cli.CLIError{
			Args: args, ExitCode: 1,
			Stderr: `Error: already exists: container "myctr"`,
		}
	}
	return a.fakeRunner.Run(ctx, args...)
}

func TestReuseAttachesToRunningContainer(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.callWith("run") != nil {
		t.Error("create run issued despite existing container")
	}
	if !ctr.reused || ctr.ID() != "myctr" {
		t.Errorf("ctr = %+v", ctr)
	}
}

func TestReuseNameConflictFallsBackToAttach(t *testing.T) {
	f := &conflictThenAttachRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
	if f.createAttempts < 1 {
		t.Error("expected a create attempt before attach")
	}
}

// TestReuseCreateNotFoundFallsBackToAttach covers Apple Container's
// concurrent-create race: run fails with "container with ID … not found"
// instead of "already exists", and reuse must re-inspect and attach.
func TestReuseCreateNotFoundFallsBackToAttach(t *testing.T) {
	f := &notFoundThenAttachRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
	if f.createAttempts < 1 {
		t.Error("expected a create attempt before attach")
	}
	if f.callWith("delete") != nil {
		t.Error("create not-found path must not delete another process's container")
	}
}

type conflictThenAttachRunner struct {
	*fakeRunner
	createAttempts int
	seenConflict   atomic.Bool
}

func (c *conflictThenAttachRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	c.mu.Lock()
	c.calls = append(c.calls, args)
	c.mu.Unlock()

	if args[0] == "image" {
		return c.fakeRunner.Run(ctx, args...)
	}
	if args[0] == "inspect" {
		if !c.seenConflict.Load() {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `inspect failed: not found: "myctr"`}
		}
		return []byte(reuseInspectJSON(args[len(args)-1], "running", "redis:7-alpine")), nil, nil
	}
	if args[0] == "run" {
		c.createAttempts++
		c.seenConflict.Store(true)
		return nil, nil, &cli.CLIError{
			Args: args, ExitCode: 1,
			Stderr: `Error: already exists: container "myctr"`,
		}
	}
	return nil, nil, nil
}

type notFoundThenAttachRunner struct {
	*fakeRunner
	createAttempts int
	seenNotFound   atomic.Bool
}

func (n *notFoundThenAttachRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	n.mu.Lock()
	n.calls = append(n.calls, args)
	n.mu.Unlock()

	if args[0] == "image" {
		return n.fakeRunner.Run(ctx, args...)
	}
	if args[0] == "inspect" {
		if !n.seenNotFound.Load() {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `inspect failed: not found: "myctr"`}
		}
		return []byte(reuseInspectJSON(args[len(args)-1], "running", "redis:7-alpine")), nil, nil
	}
	if args[0] == "run" {
		n.createAttempts++
		n.seenNotFound.Store(true)
		return nil, nil, &cli.CLIError{
			Args: args, ExitCode: 1,
			Stderr: "Error: container with ID myctr not found\n",
		}
	}
	return nil, nil, nil
}

func TestReuseRecreatesStoppedContainer(t *testing.T) {
	f := &stoppedThenCreateRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.deleted {
		t.Error("stopped container was not deleted")
	}
	if !f.created {
		t.Error("replacement container was not created")
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
}

type stoppedThenCreateRunner struct {
	*fakeRunner
	deleted  bool
	created  bool
	phase    int
	creation string
}

func (s *stoppedThenCreateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, args)
	s.mu.Unlock()

	if args[0] == "image" {
		return s.fakeRunner.Run(ctx, args...)
	}
	switch args[0] {
	case "inspect":
		if s.phase == 0 {
			return []byte(reuseInspectJSON(args[len(args)-1], "stopped", "redis:7-alpine")), nil, nil
		}
		if !s.created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
		}
		return []byte(reuseInspectJSONWithCreation(args[len(args)-1], "running", "redis:7-alpine", s.creation)), nil, nil
	case "delete":
		s.deleted = true
		s.phase = 1
		return nil, nil, nil
	case "run":
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if creation, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					s.creation = creation
				}
			}
		}
		s.created = true
		return []byte("myctr\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestReuseRunningNotReadyDoesNotDelete(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: errors.New("never ready")}),
	)
	if err == nil {
		t.Fatal("want readiness error")
	}
	if !strings.Contains(err.Error(), "failed to become ready") {
		t.Errorf("error = %v", err)
	}
	if f.callWith("delete") != nil {
		t.Error("running-not-ready path must not delete the shared container")
	}
}

func TestReuseImageMismatch(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	bad := &mismatchRunner{attachRunner: f, image: "nginx:alpine"}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(bad), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want image mismatch", err)
	}
}

type mismatchRunner struct {
	*attachRunner
	image string
}

func (m *mismatchRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		m.mu.Lock()
		m.calls = append(m.calls, args)
		m.mu.Unlock()
		return []byte(reuseInspectJSON(args[len(args)-1], "running", m.image)), nil, nil
	}
	return m.attachRunner.Run(ctx, args...)
}

func TestReuseWaitsForCreatedState(t *testing.T) {
	f := &createdThenRunningRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	start := time.Now()
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(),
		withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) < reusePollInterval {
		t.Error("expected at least one created-state poll")
	}
	if ctr.ID() != "myctr" {
		t.Errorf("id = %q", ctr.ID())
	}
}

type createdThenRunningRunner struct {
	*fakeRunner
	inspects int
}

func (c *createdThenRunningRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		c.mu.Lock()
		c.calls = append(c.calls, args)
		c.inspects++
		n := c.inspects
		c.mu.Unlock()
		state := "created"
		if n >= 2 {
			state = "running"
		}
		return []byte(reuseInspectJSON(args[len(args)-1], state, "redis:7-alpine")), nil, nil
	}
	if args[0] == "run" {
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `already exists`}
	}
	return c.fakeRunner.Run(ctx, args...)
}

func TestImagesCompatible(t *testing.T) {
	cases := []struct {
		req, act string
		ok       bool
	}{
		{"redis:7-alpine", "redis:7-alpine", true},
		{"redis:7-alpine", "docker.io/library/redis:7-alpine", true},
		{"redis:7-alpine", "nginx:alpine", false},
		{"redis:7-alpine", "redis:7-alpine@sha256:abc", true},
		{"redis:7-alpine@sha256:abc", "redis:7-alpine@sha256:abc", true},
		{"redis:7-alpine@sha256:abc", "docker.io/library/redis:7-alpine@sha256:abc", true},
		{"redis:7-alpine@sha256:new", "redis:7-alpine@sha256:old", false},
		{"redis:7-alpine@sha256:abc", "redis:7-alpine", false},
		{"evil/redis:7-alpine", "library/redis:7-alpine", false},
		{"evil/redis:7-alpine", "docker.io/library/redis:7-alpine", false},
		{"myorg/app:v1", "otherorg/app:v1", false},
	}
	for _, tc := range cases {
		if got := imagesCompatible(tc.req, tc.act); got != tc.ok {
			t.Errorf("imagesCompatible(%q,%q) = %v, want %v", tc.req, tc.act, got, tc.ok)
		}
	}
}

func TestPruneReuseGroupRemovesLabeled(t *testing.T) {
	const lsJSON = `[
  {"id":"g1","configuration":{"labels":{"com.github.hirokazumiyaji.container-go.reuse-group":"integration"}},"status":{"state":"running","networks":[]}},
  {"id":"g2","configuration":{"labels":{"com.github.hirokazumiyaji.container-go.reuse-group":"other"}},"status":{"state":"running","networks":[]}},
  {"id":"g3","configuration":{"labels":{}},"status":{"state":"stopped","networks":[]}}
]`
	f := &lsRunner{fakeRunner: newTestRunner(), lsJSON: lsJSON}
	removed, err := pruneReuseGroupWith(context.Background(), f, appleEngine{}, "integration")
	if err != nil {
		t.Fatalf("PruneReuseGroup: %v", err)
	}
	if !slices.Equal(removed, []string{"g1"}) {
		t.Errorf("removed = %v, want [g1]", removed)
	}
}

func TestAppleNameConflict(t *testing.T) {
	err := &cli.CLIError{Stderr: `Error: already exists: container "x"`}
	if !(appleEngine{}).nameConflict(err) {
		t.Error("want nameConflict")
	}
}

func TestDockerNameConflict(t *testing.T) {
	err := &cli.CLIError{Stderr: `Conflict. The container name "/x" is already in use by container`}
	if !(dockerEngine{}).nameConflict(err) {
		t.Error("want nameConflict")
	}
}
