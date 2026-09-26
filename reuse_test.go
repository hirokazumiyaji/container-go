package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: fmt.Sprintf("inspect failed: not found: %q", args[len(args)-1])}
		}
		if creation == "" {
			creation = "0123456789abcdef"
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
	return reuseInspectJSONWithCreation(id, state, image, "0123456789abcdef")
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
	deleted bool
	created bool
	phase   int
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
		return []byte(reuseInspectJSON(args[len(args)-1], "running", "redis:7-alpine")), nil, nil
	case "delete":
		s.deleted = true
		s.phase = 1
		return nil, nil, nil
	case "run":
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

type reuseRollbackRunner struct {
	*fakeRunner
	created      bool
	inspectCalls int
	deleteCalls  int
	inspectErr   error
	copyErr      error
	deleteErr    error
}

func (r *reuseRollbackRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.created = true
		r.mu.Unlock()
		return r.fakeRunner.Run(ctx, args...)
	case "inspect":
		id := args[len(args)-1]
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspectCalls++
		inspectCalls := r.inspectCalls
		created := r.created
		creation := r.creations[id]
		r.mu.Unlock()
		if !created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: fmt.Sprintf("not found: %q", id)}
		}
		if inspectCalls == 2 && r.inspectErr != nil {
			return nil, nil, r.inspectErr
		}
		return []byte(fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.session": %q,
        "com.github.hirokazumiyaji.container-go.creation": %q,
        "com.github.hirokazumiyaji.container-go.reuse": "true"
      }
    },
    "status": {"state": "running", "networks": []}
  }
]`, id, id, sessionID(), creation)), nil, nil
	case "cp":
		if r.copyErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.copyErr
		}
	case "delete":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleteCalls++
		deleteErr := r.deleteErr
		r.mu.Unlock()
		if deleteErr != nil {
			return nil, nil, deleteErr
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

// A reuse generation created by this call is owned by this call until it is
// published, so a post-create failure must remove it. Leaving it behind would
// let the next attach adopt a half-configured container as ready.
func TestReuseInspectFailureRemovesOwnGenerationAndPreservesErrors(t *testing.T) {
	inspectErr := &cli.CLIError{Args: []string{"inspect", "reuse-rollback"}, ExitCode: 1, Stderr: "post-create inspect failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "reuse-rollback"}, ExitCode: 1, Stderr: "reuse cleanup failed"}
	r := &reuseRollbackRunner{
		fakeRunner: newTestRunner(),
		inspectErr: inspectErr,
		deleteErr:  cleanupErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("reuse-rollback"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if got := cliErrorWithStderr(err, inspectErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original inspect CLIError", err)
	}
	if strings.Contains(err.Error(), "refusing automatic deletion") {
		t.Fatalf("error = %v, creator must not refuse to remove its own generation", err)
	}
	// The creator attempted the delete, and that delete failed, so both
	// errors must be recoverable from the chain.
	if r.deleteCalls == 0 {
		t.Fatal("creator did not attempt to remove its own generation")
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want the cleanup failure preserved too", err)
	}
}

func TestReuseCopyFailureRemovesOwnGenerationAndPreservesErrors(t *testing.T) {
	copyErr := &cli.CLIError{Args: []string{"cp"}, ExitCode: 1, Stderr: "reuse copy failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "reuse-copy-rollback"}, ExitCode: 1, Stderr: "reuse cleanup failed"}
	r := &reuseRollbackRunner{
		fakeRunner: newTestRunner(),
		copyErr:    copyErr,
		deleteErr:  cleanupErr,
	}
	hostPath := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(hostPath, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("reuse-copy-rollback"), WithReuse(), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: hostPath, ContainerPath: "/tmp/input.txt"}))
	if got := cliErrorWithStderr(err, copyErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original copy CLIError", err)
	}
	if strings.Contains(err.Error(), "refusing automatic deletion") {
		t.Fatalf("error = %v, creator must not refuse to remove its own generation", err)
	}
	if r.deleteCalls == 0 {
		t.Fatal("creator did not attempt to remove its own generation")
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want the cleanup failure preserved too", err)
	}
}

// An attacher that did not create the generation shares it, so its rollback
// must not delete it.
func TestReuseAttacherDoesNotDeleteSharedGeneration(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.reused = true
	ctr.creator = false

	cause := errors.New("copy failed")
	err := ctr.rollback(context.Background(), cause)
	if err == nil {
		t.Fatal("rollback returned nil")
	}
	if !errors.Is(err, cause) {
		t.Errorf("original cause lost: %v", err)
	}
	if !strings.Contains(err.Error(), "refusing automatic deletion") {
		t.Errorf("error = %v, want shared-generation cleanup refusal", err)
	}
	if call := f.callWith("delete"); call != nil {
		t.Errorf("attacher deleted a shared generation: %v", call)
	}
}

// The creator of a reuse generation may remove it, so its rollback is not
// blocked by the shared-handle refusal.
func TestReuseCreatorMayRemoveOwnGeneration(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.reused = true
	ctr.creator = true

	cause := errors.New("copy failed")
	err := ctr.rollback(context.Background(), cause)
	if err == nil {
		t.Fatal("rollback returned nil")
	}
	if !errors.Is(err, cause) {
		t.Errorf("original cause lost: %v", err)
	}
	if strings.Contains(err.Error(), "refusing automatic deletion") {
		t.Errorf("creator refused to remove its own generation: %v", err)
	}
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
  {"id":"g1","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"aaaaaaaaaaaaaaaa","com.github.hirokazumiyaji.container-go.reuse-group":"integration"}},"status":{"state":"running","networks":[]}},
  {"id":"g2","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"bbbbbbbbbbbbbbbb","com.github.hirokazumiyaji.container-go.reuse-group":"other"}},"status":{"state":"running","networks":[]}},
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
	err := &cli.CLIError{Args: []string{"run", "--name", "x"}, Stderr: `Error: already exists: container "x"`}
	if !(appleEngine{}).nameConflict(err) {
		t.Error("want nameConflict")
	}
}

func TestDockerNameConflict(t *testing.T) {
	err := &cli.CLIError{Binary: "docker", Args: []string{"run", "--name", "x"}, Stderr: `Conflict. The container name "/x" is already in use by container`}
	if !(dockerEngine{}).nameConflict(err) {
		t.Error("want nameConflict")
	}
}
