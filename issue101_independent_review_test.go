package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reviewRunner models one named container whose reported state, labels
// and per-operation failures the test controls. Every test below drives
// an exact CLI sequence through it.
type reviewRunner struct {
	*fakeRunner

	// mutable container state
	existing bool
	created  bool
	state    string
	image    string
	labels   map[string]string
	// keepPresetLabels reports the preset labels verbatim instead of
	// stamping the generation the run passed on the CLI. It models a
	// same-name replacement that already owns the name.
	keepPresetLabels bool
	uid              string
	deleted          []string
	// runCount counts create attempts and is never reset, so a test can
	// assert that a create happened even after cleanup removed it.
	runCount int

	runErr      error
	inspectErr  error
	inspectErrs map[int]error
	inspectCall int
	copyErr     error
	deleteErr   error
	probeErr    error
}

func newReviewRunner() *reviewRunner {
	return &reviewRunner{
		fakeRunner: newTestRunner(),
		state:      "running",
		image:      "redis:7-alpine",
	}
}

func (r *reviewRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, nil
	}
	switch args[0] {
	case "system", "version":
		if r.probeErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.probeErr
		}
		return []byte("ok"), nil, nil
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.created = true
		r.runCount++
		name := ""
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				name = args[i+1]
			}
			if a == "--label" && i+1 < len(args) {
				if v, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					if r.creations == nil {
						r.creations = map[string]string{}
					}
					r.creations[name] = v
				}
			}
		}
		if r.labels == nil {
			r.labels = map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				reuseLabel:    "true",
				creationLabel: r.creations[name],
			}
		} else if !r.keepPresetLabels && r.creations[name] != "" {
			labels := map[string]string{}
			for k, v := range r.labels {
				labels[k] = v
			}
			labels[creationLabel] = r.creations[name]
			r.labels = labels
		}
		err := r.runErr
		r.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		if r.uid != "" {
			return []byte(r.uid + "\n"), nil, nil
		}
		return []byte(args[len(args)-1] + "\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspectCall++
		n := r.inspectCall
		id := args[len(args)-1]
		existing, created := r.existing, r.created
		state, image, labels, uid := r.state, r.image, r.labels, r.uid
		inspectErr, inspectErrs := r.inspectErr, r.inspectErrs
		r.mu.Unlock()
		if err := inspectErrs[n]; err != nil {
			return nil, nil, err
		}
		if inspectErr != nil {
			return nil, nil, inspectErr
		}
		if !existing && !created {
			return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: fmt.Sprintf("not found: %q", id)}
		}
		if labels == nil {
			labels = map[string]string{managedLabel: "true", sessionLabel: sessionID()}
			if r.creations != nil {
				labels[creationLabel] = r.creations[id]
			}
		}
		if uid != "" {
			return []byte(dockerInspectJSON(uid, state, image, labels)), nil, nil
		}
		return []byte(inspectJSONWithStateAndLabels(id, state, image, labels)), nil, nil
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		err := r.copyErr
		r.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = append(r.deleted, args[len(args)-1])
		if r.deleteErr != nil {
			err := r.deleteErr
			r.mu.Unlock()
			return nil, nil, err
		}
		r.existing = false
		r.created = false
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func dockerInspectJSON(uid, state, image string, labels map[string]string) string {
	labelJSON, _ := json.Marshal(labels)
	return fmt.Sprintf(`[{
  "Id": %q,
  "Name": "/myctr",
  "State": {"Status": %q},
  "Config": {"Image": %q, "Labels": %s},
  "NetworkSettings": {"IPAddress": "172.17.0.2", "Ports": {}}
}]`, uid, dockerStateName(state), image, labelJSON)
}

func dockerStateName(state string) string {
	switch state {
	case "created":
		return "created"
	case "running":
		return "running"
	case "stopped":
		return "exited"
	case "stopping":
		return "removing"
	default:
		return state
	}
}

func (r *reviewRunner) deleteCalls() int { return len(r.deleted) }

func writeReviewFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- (1) rollback must not force-delete a published shared generation ---

func TestReusePostCreateFailureDoesNotForceDeletePublishedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		copy  bool
	}{
		{name: "running inspect failure", state: "running"},
		{name: "running copy failure", state: "running", copy: true},
		{name: "created copy failure", state: "created", copy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRunner()
			r.imagePresent = true
			r.state = tc.state
			cfg := &config{runner: r, eng: appleEngine{}, name: "shared", reuse: true, creation: "0123456789abcdef"}
			if tc.copy {
				r.copyErr = errors.New("injected copy failure")
				cfg.files = []File{{HostPath: writeReviewFile(t), ContainerPath: "/input"}}
			} else {
				r.inspectErrs = map[int]error{1: errors.New("injected post-create inspect failure")}
			}
			ctr, err := reuseCreate(context.Background(), "redis:7-alpine", cfg)
			if err == nil {
				t.Fatal("reuseCreate succeeded, want post-create failure")
			}
			if tc.state == "running" && r.deleteCalls() != 0 {
				t.Fatalf("deleted = %v, want no automatic delete of a published shared generation", r.deleted)
			}
			if ctr != nil && !keepContainers() {
				t.Fatalf("container = %#v, want nil handle when the shared generation is left behind", ctr)
			}
		})
	}
}

// --- (2) failed-create cleanup refuses ambiguous/running reuse generations ---

func reuseOwnedLabels(creation string) map[string]string {
	return map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		reuseLabel:    "true",
		creationLabel: creation,
	}
}

func TestFailedCreateCleanupRefusesRunningReuseGeneration(t *testing.T) {
	const creation = "0123456789abcdef"
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	for _, state := range []State{StateRunning, StateStopping, StateUnknown} {
		t.Run(string(state), func(t *testing.T) {
			r := &failRunRunner{
				fakeRunner:  newTestRunner(),
				runErr:      runErr,
				inspectJSON: inspectJSONWithStateAndLabels("myctr", string(state), "redis:7-alpine", reuseOwnedLabels(creation)),
			}
			r.creation = creation
			cfg := &config{runner: r, eng: appleEngine{}, name: "myctr", reuse: true, creation: creation}
			if err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr); err == nil {
				t.Fatalf("cleanupFailedCreate = nil, want refusal for a %s generation", state)
			}
			if len(r.deleted) != 0 {
				t.Fatalf("deleted = %v, want no delete for a %s reuse generation", r.deleted, state)
			}
		})
	}
}

// The refusal must reach the caller through CleanupError.
func TestFailedCreateCleanupRefusalPreservedInCleanupError(t *testing.T) {
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	r := newReviewRunner()
	r.imagePresent = true
	// The run fails, but the generation this process created is already
	// running: a peer may have adopted it.
	r.runErr = runErr
	r.state = "running"

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if ctr != nil {
		t.Fatalf("container = %#v, want nil", ctr)
	}
	if err == nil {
		t.Fatal("Run succeeded, want failure")
	}
	var joined *CleanupError
	if !errors.As(err, &joined) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if !strings.Contains(joined.CleanupErr.Error(), "refus") {
		t.Fatalf("cleanup error = %v, want explicit refusal", joined.CleanupErr)
	}
	if r.deleteCalls() != 0 {
		t.Fatalf("deleted = %v, want no delete of a running reuse generation", r.deleted)
	}
}

func TestFailedCreateCleanupRemovesUnadoptedReuseGeneration(t *testing.T) {
	const creation = "0123456789abcdef"
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	for _, state := range []string{"created", "stopped"} {
		t.Run(state, func(t *testing.T) {
			r := &failRunRunner{
				fakeRunner:  newTestRunner(),
				runErr:      runErr,
				inspectJSON: inspectJSONWithStateAndLabels("myctr", state, "redis:7-alpine", reuseOwnedLabels(creation)),
			}
			r.creation = creation
			cfg := &config{runner: r, eng: appleEngine{}, name: "myctr", reuse: true, creation: creation}
			if err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr); err != nil {
				t.Fatalf("cleanupFailedCreate: %v", err)
			}
			if len(r.deleted) != 1 {
				t.Fatalf("deleted = %v, want [myctr]", r.deleted)
			}
		})
	}
}

// --- (3) KEEP must not publish unverified partial handles ---

func TestKeepDropsUnverifiablePostCreateHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	r := newReviewRunner()
	r.imagePresent = true
	r.copyErr = errors.New("injected copy failure")
	// Every inspect after the copy failure fails, so the handle cannot be
	// revalidated.
	r.inspectErr = errors.New("injected verification failure")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: writeReviewFile(t), ContainerPath: "/input"}))
	if err == nil {
		t.Fatal("Run succeeded, want copy failure")
	}
	if ctr != nil {
		t.Fatalf("container = %#v, want nil for an unverifiable retained handle", ctr)
	}
	var joined *CleanupError
	if !errors.As(err, &joined) {
		t.Fatalf("error = %v, want CleanupError carrying the verification failure", err)
	}
	if !strings.Contains(joined.CleanupErr.Error(), "injected verification failure") {
		t.Fatalf("cleanup error = %v, want the verification failure", joined.CleanupErr)
	}
	if r.deleteCalls() != 0 {
		t.Fatalf("deleted = %v, want no delete under KEEP", r.deleted)
	}
}

func TestKeepDropsReplacedGenerationHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	r := newReviewRunner()
	r.imagePresent = true
	r.copyErr = errors.New("injected copy failure")
	// The live container carries a different generation than the one
	// this run created: a same-name replacement won the name.
	r.keepPresetLabels = true
	r.labels = map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		creationLabel: "aaaaaaaaaaaaaaaa",
	}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: writeReviewFile(t), ContainerPath: "/input"}))
	if err == nil {
		t.Fatal("Run succeeded, want copy failure")
	}
	if ctr != nil {
		t.Fatalf("container = %#v, want nil: the live generation was replaced", ctr)
	}
	if r.deleteCalls() != 0 {
		t.Fatalf("deleted = %v, want no delete under KEEP", r.deleted)
	}
}

// A KEEP handle whose identity still verifies must still be returned:
// that is the issue-101 contract.
func TestKeepReturnsVerifiedPostCreateHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	r := newReviewRunner()
	r.imagePresent = true
	r.copyErr = errors.New("injected copy failure")
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: writeReviewFile(t), ContainerPath: "/input"}))
	if err == nil {
		t.Fatal("Run succeeded, want copy failure")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want the verified retained handle", ctr)
	}
	if !creationRE.MatchString(ctr.creation) {
		t.Fatalf("handle creation = %q, want the run generation", ctr.creation)
	}
}

// --- (4) retry classification only reads the primary operation ---

func TestReuseRetryIgnoresCleanupBranchConflict(t *testing.T) {
	// A retry bug would spin until this attach deadline; a correct run
	// returns the primary failure immediately.
	oldAttach := reuseAttachTimeout
	reuseAttachTimeout = 2 * time.Second
	defer func() { reuseAttachTimeout = oldAttach }()

	primary := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "reuse primary failure"}
	// The cleanup failure looks like a name conflict. It must not turn a
	// terminal primary failure into an attach retry loop.
	cleanup := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: `Error: already exists: container "myctr"`}
	base := newTestRunner()
	base.imagePresent = true
	inner := &failRunRunner{
		fakeRunner:  base,
		runErr:      primary,
		inspectJSON: inspectJSONWithStateAndLabels("myctr", "created", "redis:7-alpine", reuseOwnedLabels("__CONTAINER_CREATION__")),
		deleteErr:   cleanup,
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: new(int)}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if ctr != nil {
		t.Fatalf("container = %#v, want nil", ctr)
	}
	if err == nil {
		t.Fatal("Run succeeded, want the primary failure")
	}
	if got := cliErrorWithStderr(err, primary.Stderr); got == nil {
		t.Fatalf("error = %v, want the primary CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanup.Stderr); got == nil {
		t.Fatalf("error = %v, want the cleanup CLIError preserved", err)
	}
	var joined *CleanupError
	if !errors.As(err, &joined) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if primaryErr := primaryOperationError(err); !errors.Is(primaryErr, primary) {
		t.Fatalf("primaryOperationError = %v, want %v", primaryErr, primary)
	}
}

// --- (6) post-create compatibility failure ---

func TestReusePostCreateCompatibilityFailureCleansCreatedGeneration(t *testing.T) {
	r := newReviewRunner()
	r.imagePresent = true
	// The created generation never bound the published port, so the
	// compatibility check fails after create.
	r.state = "created"
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithPublishedPort("127.0.0.1:16379:6379/tcp"),
		withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded, want a compatibility failure")
	}
	if ctr != nil {
		t.Fatalf("container = %#v, want nil", ctr)
	}
	if r.runCount == 0 {
		t.Fatal("test did not create a container")
	}
	if r.deleteCalls() == 0 {
		t.Fatalf("deleted = %v, want the unadopted created generation removed", r.deleted)
	}
}

func TestReusePostCreateCompatibilityFailureKeepsAdoptedGeneration(t *testing.T) {
	r := newReviewRunner()
	r.imagePresent = true
	r.state = "running"
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithPublishedPort("127.0.0.1:16379:6379/tcp"),
		withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded, want a compatibility failure")
	}
	if ctr != nil {
		t.Fatalf("container = %#v, want nil", ctr)
	}
	if r.deleteCalls() != 0 {
		t.Fatalf("deleted = %v, want no delete of an adopted generation", r.deleted)
	}
}

// --- (7) stopped reuse replacement re-checks state under the lock ---

func TestDeleteStoppedReuseRechecksStoppedStateUnderLock(t *testing.T) {
	const creation = "0123456789abcdef"
	labels := map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: creation,
	}
	r := &stoppedThenRunningRunner{labels: labels, states: []string{"running"}}
	cfg := &config{runner: r, eng: appleEngine{}, name: "shared"}
	info := &engineInfo{state: StateStopped, labels: labels}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse = %v, want the running generation left alone", err)
	}
	if r.deleteCall != 0 {
		t.Fatalf("delete calls = %d, want 0 once the generation is running again", r.deleteCall)
	}
}

// A stopped generation that is still stopped is replaced as before.
func TestDeleteStoppedReuseDeletesStillStoppedGeneration(t *testing.T) {
	const creation = "0123456789abcdef"
	labels := map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: creation,
	}
	r := &stoppedThenRunningRunner{labels: labels, states: []string{"stopped"}}
	cfg := &config{runner: r, eng: appleEngine{}, name: "shared"}
	info := &engineInfo{state: StateStopped, labels: labels}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse: %v", err)
	}
	if r.deleteCall != 1 {
		t.Fatalf("delete calls = %d, want 1 for the still-stopped generation", r.deleteCall)
	}
	if len(r.deleted) != 1 || r.deleted[0] != "shared" {
		t.Fatalf("deleted = %v, want the name-addressed Apple delete", r.deleted)
	}
}

// A stopped Docker generation is deleted by the immutable ID the
// re-inspect reported, never by the shared name.
func TestDeleteStoppedReuseDockerUsesImmutableID(t *testing.T) {
	const creation = "0123456789abcdef"
	uid := strings.Repeat("ab", 32)
	labels := map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: creation,
	}
	r := &dockerStoppedReuseRunner{uid: uid, labels: labels}
	cfg := &config{runner: r, eng: dockerEngine{}, name: "shared"}
	info := &engineInfo{state: StateStopped, labels: labels, uid: uid}
	if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
		t.Fatalf("deleteStoppedReuse: %v", err)
	}
	if len(r.deleted) != 1 || r.deleted[0] != uid {
		t.Fatalf("deleted = %v, want the immutable ID %q", r.deleted, uid)
	}
}

type dockerStoppedReuseRunner struct {
	uid     string
	labels  map[string]string
	deleted []string
}

func (d *dockerStoppedReuseRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, nil
	}
	switch args[0] {
	case "inspect":
		return []byte(dockerInspectJSON(d.uid, "stopped", "redis:7-alpine", d.labels)), nil, nil
	case "rm", "delete":
		d.deleted = append(d.deleted, args[len(args)-1])
		return nil, nil, nil
	default:
		return []byte("ok"), nil, nil
	}
}

// stoppedThenRunningRunner serves the states the test declares in
// order, one per inspect: the first models what a peer restart looked
// like when the deletion lock was taken.
type stoppedThenRunningRunner struct {
	labels     map[string]string
	states     []string
	deleteCall int
	inspects   int
	deleted    []string
}

func (s *stoppedThenRunningRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, nil
	}
	switch args[0] {
	case "inspect":
		state := "stopped"
		if s.inspects < len(s.states) {
			state = s.states[s.inspects]
		}
		s.inspects++
		return []byte(inspectJSONWithStateAndLabels(args[len(args)-1], state, "redis:7-alpine", s.labels)), nil, nil
	case "delete", "rm":
		s.deleteCall++
		s.deleted = append(s.deleted, args[len(args)-1])
		return nil, nil, nil
	case "system", "version":
		return []byte("ok"), nil, nil
	default:
		return nil, nil, nil
	}
}

// --- (8) ErrSystemNotRunning must not become a not-found ---

func TestSystemNotRunningWrapperIsNotContainerNotFound(t *testing.T) {
	nested := &cli.CLIError{
		Binary: "container", Args: []string{"delete", "--force", "myctr"},
		ExitCode: 1, Stderr: `delete failed: not found: "myctr"`,
	}
	wrapped := fmt.Errorf("%w: %s (underlying error: %w)", ErrSystemNotRunning, "run `container system start`", nested)
	if isNotFound(wrapped) {
		t.Fatal("isNotFound = true for an ErrSystemNotRunning wrapper, want false")
	}
	if !isNotFound(nested) {
		t.Fatal("isNotFound = false for a direct not-found delete, want true")
	}

	// Operation/target scope: a not-found for another container is not
	// evidence that this delete removed the requested target.
	other := &cli.CLIError{
		Binary: "container", Args: []string{"delete", "--force", "other"},
		ExitCode: 1, Stderr: `delete failed: not found: "other"`,
	}
	if isDeleteNotFound(appleEngine{}, "myctr", other) {
		t.Fatal("isDeleteNotFound = true for a different target, want false")
	}
	if !isDeleteNotFound(appleEngine{}, "other", other) {
		t.Fatal("isDeleteNotFound = false for the matching target, want true")
	}
}

func TestDeletePreservesSystemNotRunningFailure(t *testing.T) {
	base := newTestRunner()
	ctr := &Container{
		id: "myctr", runner: &systemDownRunner{fakeRunner: base},
		eng: appleEngine{}, creation: "0123456789abcdef",
	}
	err := ctr.delete(context.Background(), "myctr")
	if err == nil {
		t.Fatal("delete = nil, want the system-down failure preserved")
	}
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
}

// A missing container still reports ErrContainerNotFound, and the same
// stderr wrapped as a backend-down failure never does.
func TestMissingContainerStillReportsNotFound(t *testing.T) {
	missing := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"},
		ExitCode: 1, Stderr: `Error: No such object: myctr`,
	}
	ctr := &Container{
		id:     "myctr",
		runner: &singleErrorRunner{fakeRunner: newTestRunner(), err: missing},
		eng:    dockerEngine{},
	}
	if _, err := ctr.State(context.Background()); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want ErrContainerNotFound", err)
	}

	down := &singleErrorRunner{fakeRunner: newTestRunner(), err: missing}
	down.probeErr = &cli.CLIError{Args: []string{"version", "--format", "x"}, ExitCode: 1, Stderr: "cannot connect"}
	ctrDown := &Container{id: "myctr", runner: down, eng: dockerEngine{}}
	err := ctrDown.State(context.Background())
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want the backend-down failure, not absence", err)
	}
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("State error = %v, want ErrSystemNotRunning", err)
	}
}

// singleErrorRunner fails inspect with a fixed error and answers the
// liveness probe with probeErr.
type singleErrorRunner struct {
	*fakeRunner
	err      error
	probeErr error
}

func (s *singleErrorRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && (args[0] == "version" || args[0] == "system") && s.probeErr != nil {
		return nil, nil, s.probeErr
	}
	if len(args) > 0 && args[0] == "inspect" && s.err != nil {
		return nil, nil, s.err
	}
	return s.fakeRunner.Run(ctx, args...)
}

type systemDownRunner struct {
	*fakeRunner
}

func (s *systemDownRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && (args[0] == "delete" || args[0] == "rm") {
		nested := &cli.CLIError{
			Binary: "container", Args: args,
			ExitCode: 1, Stderr: `delete failed: not found: "myctr"`,
		}
		return nil, nil, fmt.Errorf("%w: %s (underlying error: %w)", ErrSystemNotRunning, "run `container system start`", nested)
	}
	return s.fakeRunner.Run(ctx, args...)
}
