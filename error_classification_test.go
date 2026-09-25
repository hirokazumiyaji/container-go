package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type backendStderrFixture struct {
	Name             string `json:"name"`
	Operation        string `json:"operation"`
	Stderr           string `json:"stderr"`
	NameConflict     bool   `json:"nameConflict"`
	ImageMissing     bool   `json:"imageMissing"`
	ContainerMissing bool   `json:"containerMissing"`
}

type backendStderrClassifier struct {
	nameConflict     func(error) bool
	imageMissing     func(error) bool
	containerMissing func(error) bool
}

func loadBackendStderrFixtures(t *testing.T, file string) []backendStderrFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []backendStderrFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("%s contains no fixtures", file)
	}
	return fixtures
}

func fixtureArgs(operation string) []string {
	switch operation {
	case "image inspect":
		return []string{"image", "inspect", "redis:7-alpine"}
	case "run":
		return []string{"run", "--name", "myctr"}
	case "rm":
		return []string{"rm", "--force", "myctr"}
	default:
		return []string{operation, "myctr"}
	}
}

// The fixture files contain the verified stderr shapes from the supported
// CLI versions; the table keeps each classifier's positive and negative
// contract together.
func TestBackendStderrFixtures(t *testing.T) {
	cases := []struct {
		name       string
		backend    string
		file       string
		classifier backendStderrClassifier
	}{
		{
			name:    "apple-1.2",
			backend: "apple",
			file:    "cli_stderr_apple_1.2.0.json",
			classifier: backendStderrClassifier{
				nameConflict:     appleEngine{}.nameConflict,
				imageMissing:     appleEngine{}.imageMissing,
				containerMissing: appleEngine{}.containerMissing,
			},
		},
		{
			name:    "apple-1.3",
			backend: "apple",
			file:    "cli_stderr_apple_1.3.0.json",
			classifier: backendStderrClassifier{
				nameConflict:     appleEngine{}.nameConflict,
				imageMissing:     appleEngine{}.imageMissing,
				containerMissing: appleEngine{}.containerMissing,
			},
		},
		{
			name:    "docker-29",
			backend: "docker",
			file:    "cli_stderr_docker_29.json",
			classifier: backendStderrClassifier{
				nameConflict:     dockerEngine{}.nameConflict,
				imageMissing:     dockerEngine{}.imageMissing,
				containerMissing: dockerEngine{}.containerMissing,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, fixture := range loadBackendStderrFixtures(t, tc.file) {
				t.Run(fixture.Name, func(t *testing.T) {
					binary := "container"
					if tc.backend == "docker" {
						binary = "docker"
					}
					err := &cli.CLIError{
						Binary:   binary,
						Args:     fixtureArgs(fixture.Operation),
						ExitCode: 1,
						Stderr:   fixture.Stderr,
					}
					if got := tc.classifier.nameConflict(err); got != fixture.NameConflict {
						t.Errorf("nameConflict(%q) = %v, want %v", fixture.Stderr, got, fixture.NameConflict)
					}
					if got := tc.classifier.imageMissing(err); got != fixture.ImageMissing {
						t.Errorf("imageMissing(%q) = %v, want %v", fixture.Stderr, got, fixture.ImageMissing)
					}
					if got := tc.classifier.containerMissing(err); got != fixture.ContainerMissing {
						t.Errorf("containerMissing(%q) = %v, want %v", fixture.Stderr, got, fixture.ContainerMissing)
					}
					if got := isNotFound(err); got != fixture.ContainerMissing {
						t.Errorf("isNotFound(%q) = %v, want %v", fixture.Stderr, got, fixture.ContainerMissing)
					}
				})
			}
		})
	}
}

func TestBackendStderrClassifiersRejectAmbiguousMessages(t *testing.T) {
	messages := []string{
		"permission denied: not found",
		"image config contains a conflict",
		"address already in use",
		"application error: container not found: myctr",
		"configuration error: no such image: redis:7-alpine",
	}
	classifiers := []backendStderrClassifier{
		{
			nameConflict:     appleEngine{}.nameConflict,
			imageMissing:     appleEngine{}.imageMissing,
			containerMissing: appleEngine{}.containerMissing,
		},
		{
			nameConflict:     dockerEngine{}.nameConflict,
			imageMissing:     dockerEngine{}.imageMissing,
			containerMissing: dockerEngine{}.containerMissing,
		},
	}

	for i, classifier := range classifiers {
		binary := "container"
		if i == 1 {
			binary = "docker"
		}
		for _, stderr := range messages {
			err := &cli.CLIError{Binary: binary, Args: []string{"run"}, ExitCode: 1, Stderr: stderr}
			if classifier.nameConflict(err) || classifier.imageMissing(err) || classifier.containerMissing(err) || isNotFound(err) {
				t.Errorf("classifier %d classified %q", i, stderr)
			}
		}
	}
}

func TestAmbiguousStderrPreservesOriginalCLIError(t *testing.T) {
	original := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"inspect", "myctr"},
		ExitCode: 1,
		Stderr:   "permission denied: not found",
	}
	if got := wrapNotFound(original); got != original {
		t.Fatalf("wrapNotFound() = %v, want original CLIError", got)
	}

	wrapped := errors.Join(original, errors.New("diagnostic context"))
	if got := wrapNotFound(wrapped); got != wrapped {
		t.Fatalf("wrapNotFound(wrapped) = %v, want wrapped original", got)
	}
}

func TestImageInspectPermissionErrorPreservesCLIError(t *testing.T) {
	cases := []struct {
		name   string
		engine engine
		stderr string
	}{
		{
			name:   "apple",
			engine: appleEngine{},
			stderr: "permission denied: image not found: redis:7-alpine",
		},
		{
			name:   "docker",
			engine: dockerEngine{},
			stderr: "permission denied: no such image: redis:7-alpine",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := &cli.CLIError{
				Binary:   tc.name,
				Args:     []string{"image", "inspect", "redis:7-alpine"},
				ExitCode: 1,
				Stderr:   tc.stderr,
			}
			base := newTestRunner()
			r := &imageInspectErrorRunner{fakeRunner: base, err: original}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithPullPolicy(PullMissing), withRunner(r), withEngine(tc.engine))
			if err != original {
				t.Fatalf("Run error = %v, want original CLIError", err)
			}
			if r.inspectCalls != 1 {
				t.Errorf("image inspect calls = %d, want 1", r.inspectCalls)
			}
			if base.pullCalls != 0 {
				t.Errorf("pull calls = %d, want 0", base.pullCalls)
			}
		})
	}
}

type imageInspectErrorRunner struct {
	*fakeRunner
	err          *cli.CLIError
	inspectCalls int
}

func (r *imageInspectErrorRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		r.mu.Lock()
		r.inspectCalls++
		r.mu.Unlock()
		return nil, nil, r.err
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestConfigErrorDoesNotSkipFailedCreateCleanup(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	original := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"run"},
		ExitCode: 125,
		Stderr:   "image config contains a conflict",
	}
	id := strings.Repeat("0", 64)
	inspectJSON := fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:%q}},"NetworkSettings":{}}]`, id, managedLabel, sessionLabel, sessionID())
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      original,
		inspectJSON: inspectJSON,
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}))
	if err != original {
		t.Fatalf("Run error = %v, want original CLIError", err)
	}
	if len(r.deleted) != 1 || r.deleted[0] != id {
		t.Fatalf("deleted = %v, want owned failed create to be removed by ID", r.deleted)
	}
}

func TestReuseConfigConflictDoesNotEnterAttachLoop(t *testing.T) {
	oldTimeout, oldPoll := reuseAttachTimeout, reusePollInterval
	reuseAttachTimeout = 200 * time.Millisecond
	reusePollInterval = time.Millisecond
	defer func() { reuseAttachTimeout, reusePollInterval = oldTimeout, oldPoll }()

	base := newTestRunner()
	base.imagePresent = true
	original := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"run"},
		ExitCode: 125,
		Stderr:   "image config contains a conflict",
	}
	r := &ambiguousReuseRunner{fakeRunner: base, runErr: original}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("ambiguous-reuse"), WithReuse(), withRunner(r), withEngine(dockerEngine{}))
	if err != original {
		t.Fatalf("Run error = %v, want original CLIError", err)
	}
	if !strings.Contains(err.Error(), "image config contains a conflict") {
		t.Fatalf("error = %v, want configuration diagnostic", err)
	}
	if r.runCalls != 1 {
		t.Fatalf("run calls = %d, want 1", r.runCalls)
	}
}

type ambiguousReuseRunner struct {
	*fakeRunner
	runErr       error
	runCalls     int
	inspectCalls int
}

func (r *ambiguousReuseRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.runCalls++
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.inspectCalls++
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{
			Binary:   "docker",
			Args:     args,
			ExitCode: 1,
			Stderr:   `Error response from daemon: No such object: "ambiguous-reuse"`,
		}
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestAmbiguousInspectErrorPreservesCLIError(t *testing.T) {
	original := &cli.CLIError{
		Binary:   "container",
		Args:     []string{"inspect", "myctr"},
		ExitCode: 1,
		Stderr:   "permission denied: not found",
	}
	ctr := &Container{
		id:     "myctr",
		runner: &fixedInspectErrorRunner{err: original},
		eng:    appleEngine{},
	}
	_, err := ctr.State(context.Background())
	if err != original {
		t.Fatalf("State error = %v, want original CLIError", err)
	}
}

type fixedInspectErrorRunner struct {
	err *cli.CLIError
}

func (r *fixedInspectErrorRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return nil, nil, r.err
	case "system", "version":
		return []byte("running"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestClassifiersRejectCrossBackendOperationAndTarget(t *testing.T) {
	cases := []struct {
		name string
		got  bool
	}{
		{
			name: "docker wording through apple",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"inspect", "myctr"},
				Stderr: "Error response from daemon: No such container: myctr",
			}),
		},
		{
			name: "apple wording through docker",
			got: (dockerEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				Stderr: "Error: container not found: myctr",
			}),
		},
		{
			name: "empty binary is not docker",
			got: (dockerEngine{}).containerMissing(&cli.CLIError{
				Args:   []string{"inspect", "myctr"},
				Stderr: "error: no such object: myctr",
			}),
		},
		{
			name: "wrong operation",
			got: (dockerEngine{}).containerMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"delete", "myctr"},
				Stderr: "Error response from daemon: No such container: myctr",
			}),
		},
		{
			name: "wrong target",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				Stderr: "Error: container not found: other",
			}),
		},
		{
			name: "application output on run",
			got: createRaceMissing(&cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: application: container not found: myctr",
			}),
		},
		{
			name: "application output on exec",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"exec", "myctr", "true"},
				Stderr: "Error: application: container not found: myctr",
			}),
		},
		{
			name: "image wording on pull",
			got: (dockerEngine{}).imageMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"pull", "redis:7-alpine"},
				Stderr: "Error response from daemon: No such image: redis:7-alpine",
			}),
		},
		{
			name: "conflict wording on delete",
			got: (dockerEngine{}).nameConflict(&cli.CLIError{
				Binary: "docker", Args: []string{"rm", "--force", "myctr"},
				Stderr: "Conflict. The container name \"/myctr\" is already in use by container abc",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got {
				t.Fatal("ambiguous or cross-backend message was classified")
			}
		})
	}
}

func TestCreateRaceMissingIsAnchoredAndCommandAware(t *testing.T) {
	cases := []struct {
		name string
		err  *cli.CLIError
		want bool
	}{
		{
			name: "direct apple race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with ID myctr not found",
			},
			want: true,
		},
		{
			name: "wrapped apple race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: failed to bootstrap container: container with ID myctr not found",
			},
			want: true,
		},
		{
			name: "generic application message",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container not found: myctr",
			},
		},
		{
			name: "wrong command",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"exec", "myctr", "true"},
				Stderr: "Error: container with ID myctr not found",
			},
		},
		{
			name: "wrong target",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with ID other not found",
			},
		},
		{
			name: "wrong binary",
			err: &cli.CLIError{
				Binary: "docker", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with ID myctr not found",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createRaceMissing(tc.err); got != tc.want {
				t.Fatalf("createRaceMissing() = %v, want %v", got, tc.want)
			}
		})
	}
}

type ambiguousOriginalRunner struct {
	*fakeRunner
	original     *cli.CLIError
	partial      bool
	reuse        bool
	runCalls     int
	inspectCalls int
	probeCalls   int
	deleteCalls  int
}

func (r *ambiguousOriginalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.runCalls++
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.original
	case "system":
		r.mu.Lock()
		r.probeCalls++
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{
			Binary: "container", Args: args, ExitCode: 1,
			Stderr: "XPC connection error",
		}
	case "inspect":
		r.mu.Lock()
		r.inspectCalls++
		n := r.inspectCalls
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		if r.partial && ((!r.reuse && n > 0) || (r.reuse && n > 1)) {
			return []byte(ownedInspectJSON(args[len(args)-1])), nil, nil
		}
		return nil, nil, &cli.CLIError{
			Binary: "container", Args: args, ExitCode: 1,
			Stderr: "Error: container not found: " + args[len(args)-1],
		}
	case "delete":
		r.mu.Lock()
		r.deleteCalls++
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestAmbiguousRunPreservesOriginalAndCleansPartialCreate(t *testing.T) {
	cases := []struct {
		name        string
		container   string
		withReuse   bool
		partial     bool
		wantInspect int
		wantProbe   int
	}{
		{name: "ordinary run", container: "ambiguous-run-original", wantInspect: 1, wantProbe: 1},
		{name: "ordinary run partial", container: "ambiguous-run-partial", partial: true, wantInspect: 1},
		{name: "reuse no retry", container: "ambiguous-reuse-original", withReuse: true, wantInspect: 2, wantProbe: 2},
		{name: "reuse partial cleanup", container: "ambiguous-reuse-partial", withReuse: true, partial: true, wantInspect: 2, wantProbe: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestRunner()
			base.imagePresent = true
			original := &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", tc.container},
				ExitCode: 1, Stderr: "Error: application: container not found: " + tc.container,
			}
			r := &ambiguousOriginalRunner{
				fakeRunner: base, original: original, partial: tc.partial, reuse: tc.withReuse,
			}
			opts := []Option{WithName(tc.container), withRunner(r), withEngine(appleEngine{})}
			if tc.withReuse {
				opts = append(opts, WithReuse())
			}
			_, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err != original {
				t.Fatalf("Run error = %v, want original CLIError", err)
			}
			if errors.Is(err, ErrSystemNotRunning) {
				t.Fatal("ambiguous application error was classified as daemon down")
			}
			if r.runCalls != 1 {
				t.Fatalf("run calls = %d, want 1", r.runCalls)
			}
			if r.probeCalls != tc.wantProbe {
				t.Fatalf("probe calls = %d, want %d", r.probeCalls, tc.wantProbe)
			}
			if r.inspectCalls != tc.wantInspect {
				t.Fatalf("inspect calls = %d, want %d", r.inspectCalls, tc.wantInspect)
			}
			wantDeletes := 0
			if tc.partial {
				wantDeletes = 1
			}
			if r.deleteCalls != wantDeletes {
				t.Fatalf("delete calls = %d, want %d", r.deleteCalls, wantDeletes)
			}
		})
	}
}

type pruneDeleteRunner struct {
	list        string
	deleteErr   error
	deleteCalls int
}

func (r *pruneDeleteRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ls" {
		return []byte(r.list), nil, nil
	}
	if args[0] == "delete" || args[0] == "rm" {
		r.deleteCalls++
		return nil, nil, r.deleteErr
	}
	return nil, nil, nil
}

func TestCleanupDeleteClassifierIsOperationAndBackendAware(t *testing.T) {
	cases := []struct {
		name    string
		err     *cli.CLIError
		wantIDs []string
		wantErr bool
	}{
		{
			name: "apple missing delete",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"delete", "--force", "id"},
				Stderr: "Error: failed to delete container: container with ID id not found",
			},
			wantIDs: []string{"id"},
		},
		{
			name: "ambiguous delete",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"delete", "--force", "id"},
				Stderr: "Error: application: container not found: id",
			},
			wantErr: true,
		},
		{
			name: "docker delete through apple cleanup",
			err: &cli.CLIError{
				Binary: "docker", Args: []string{"rm", "--force", "id"},
				Stderr: "Error response from daemon: No such container: id",
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &pruneDeleteRunner{list: "id\n", deleteErr: tc.err}
			ids, err := pruneListed(context.Background(), r, appleEngine{}, []string{"ls"}, func(data []byte) ([]string, error) {
				return splitNonEmptyLines(data), nil
			}, "prune")
			if (err != nil) != tc.wantErr {
				t.Fatalf("prune error = %v, want error=%v", err, tc.wantErr)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("removed = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func TestImageMissingClassifierRejectsPullOperation(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker", Args: []string{"pull", "redis:7-alpine"},
		Stderr: "Error response from daemon: No such image: redis:7-alpine",
	}
	if (dockerEngine{}).imageMissing(err) {
		t.Fatal("pull output was classified as image-missing")
	}
}
