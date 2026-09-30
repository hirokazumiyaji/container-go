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

type probeOutputFixture struct {
	Name        string `json:"name"`
	Backend     string `json:"backend"`
	Operation   string `json:"operation"`
	Stream      string `json:"stream"`
	Output      string `json:"output"`
	Unavailable bool   `json:"unavailable"`
}

func loadProbeOutputFixtures(t *testing.T) []probeOutputFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/probe_output_fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []probeOutputFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode probe fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("probe fixture file is empty")
	}
	return fixtures
}

type fixtureProbeRunner struct {
	stdout string
	stderr string
	err    error
}

func (r *fixtureProbeRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	return []byte(r.stdout), []byte(r.stderr), r.err
}

func TestProductionProbeFixtures(t *testing.T) {
	for _, fixture := range loadProbeOutputFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			var eng engine
			switch fixture.Backend {
			case "apple":
				eng = appleEngine{}
			case "docker":
				eng = dockerEngine{}
			default:
				t.Fatalf("unknown fixture backend %q", fixture.Backend)
			}
			probe := eng.probe()
			wantHint := "start the Docker daemon"
			if fixture.Backend == "apple" {
				wantHint = "Ensure container system service has been started with `container system start`."
			}
			if probe.Hint != wantHint {
				t.Fatalf("probe hint = %q, want %q", probe.Hint, wantHint)
			}
			if commandOperation(probe.Args) != fixture.Operation {
				t.Fatalf("probe operation = %q, want fixture %q", commandOperation(probe.Args), fixture.Operation)
			}
			original := &cli.CLIError{
				Binary: eng.binary(), Args: []string{"run"}, ExitCode: 1, Stderr: "command failed",
			}
			probeErr := &cli.CLIError{
				Binary: eng.binary(), Args: probe.Args, ExitCode: 1, Stderr: "probe command failed",
			}
			runner := &fixtureProbeRunner{err: probeErr}
			switch fixture.Stream {
			case "stdout":
				runner.stdout = fixture.Output
			case "stderr":
				runner.stderr = fixture.Output
			default:
				t.Fatalf("unknown fixture stream %q", fixture.Stream)
			}

			got := cli.Classify(context.Background(), runner, original, probe)
			if errors.Is(got, ErrSystemNotRunning) != fixture.Unavailable {
				t.Fatalf("Classify(%q) unavailable = %v, want %v; error = %v", fixture.Output, errors.Is(got, ErrSystemNotRunning), fixture.Unavailable, got)
			}
			if fixture.Unavailable && !strings.Contains(got.Error(), probe.Hint) {
				t.Fatalf("Classify(%q) error = %v, want probe hint %q", fixture.Output, got, probe.Hint)
			}
			if !errors.Is(got, original) || !errors.Is(got, probeErr) {
				t.Fatalf("Classify error = %v, want original and probe chains", got)
			}
		})
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
			name: "wrong container target",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				Stderr: "Error: container not found: other",
			}),
		},
		{
			name: "wrong image target",
			got: (dockerEngine{}).imageMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"image", "inspect", "redis:7-alpine"},
				Stderr: "Error response from daemon: No such image: other:tag",
			}),
		},
		{
			name: "wrong conflict target",
			got: (dockerEngine{}).nameConflict(&cli.CLIError{
				Binary: "docker", Args: []string{"run", "--name", "myctr"},
				Stderr: `Conflict. The container name "/other" is already in use by container abc`,
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
			name: "application envelope on delete",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"delete", "--force", "myctr"},
				Stderr: `Error: notFound: "container with ID myctr not found"`,
			}),
		},
		{
			name: "application envelope on logs",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"logs", "myctr"},
				Stderr: `Error: notFound: "container with ID myctr not found"`,
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
			name: "real envelope race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: `Error: internalError: "failed to bootstrap container" (cause: "notFound: "container with ID myctr not found"")`,
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
			name: "application envelope without run wrapper",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: `Error: notFound: "container with ID myctr not found"`,
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

func TestParseAppleContainerizationEnvelope(t *testing.T) {
	envelope, ok := parseAppleContainerizationEnvelope(
		`Error: internalError: "failed to delete container" (cause: "notFound: "container with ID myctr not found"")`,
	)
	if !ok {
		t.Fatal("real Apple envelope did not parse")
	}
	if envelope.code != "internalerror" || envelope.message != "failed to delete container" {
		t.Fatalf("root envelope = %+v", envelope)
	}
	if envelope.cause == nil || envelope.cause.code != "notfound" ||
		envelope.cause.message != "container with id myctr not found" {
		t.Fatalf("nested envelope = %+v", envelope.cause)
	}

	for _, invalid := range []string{
		`application: notFound: "container not found: myctr"`,
		`notFound: container not found: myctr`,
		`madeUpCode: "container not found: myctr"`,
	} {
		if _, ok := parseAppleContainerizationEnvelope(invalid); ok {
			t.Errorf("accepted invalid envelope %q", invalid)
		}
	}
}

func TestCommandTargetUsesOperationSpecificArgvSchemas(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		args      []string
		want      string
	}{
		{
			name:      "apple bounded logs short option value",
			operation: "logs",
			args:      []string{"logs", "-n", "1000", "myctr"},
			want:      "myctr",
		},
		{
			name:      "public logs option values",
			operation: "logs",
			args:      []string{"logs", "--tail", "50", "--since", "2026-09-25T00:00:00Z", "myctr"},
			want:      "myctr",
		},
		{
			name:      "exec option values",
			operation: "exec",
			args:      []string{"exec", "--env-file", "/tmp/env", "--user", "nobody", "--workdir", "/work", "myctr", "true"},
			want:      "myctr",
		},
		{
			name:      "image inspect option value",
			operation: "image inspect",
			args:      []string{"image", "inspect", "--platform", "linux/amd64", "redis:7-alpine"},
			want:      "redis:7-alpine",
		},
		{
			name:      "stop option value",
			operation: "stop",
			args:      []string{"stop", "--time", "10", "myctr"},
			want:      "myctr",
		},
		{
			name:      "run name equals value",
			operation: "run",
			args:      []string{"run", "--name=myctr", "redis:7-alpine"},
			want:      "myctr",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandTarget(tc.args, tc.operation); got != tc.want {
				t.Fatalf("commandTarget(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestAppleLogsMissingAcceptsKnownBoundedWrapperVariants(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "get failed leaf",
			stderr: "Error: failed to get logs for container myctr: get failed: container myctr not found",
			want:   true,
		},
		{
			name:   "single open wrapper",
			stderr: "Error: failed to get logs for container myctr: failed to open container logs: container with ID myctr not found",
			want:   true,
		},
		{
			name:   "open wrapping get failed",
			stderr: "Error: failed to open container logs: get failed: container myctr not found",
			want:   true,
		},
		{
			name:   "bounded wrapper depth",
			stderr: "Error: failed to get logs for container myctr: failed to open container logs: failed to open container logs: failed to open container logs: failed to open container logs: container with ID myctr not found",
			want:   false,
		},
		{
			name:   "wrong target",
			stderr: "Error: failed to get logs for container myctr: get failed: container other not found",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &cli.CLIError{
				Binary: "container", Args: []string{"logs", "-n", "1000", "myctr"},
				ExitCode: 1, Stderr: tc.stderr,
			}
			if got := (appleEngine{}).containerMissing(err); got != tc.want {
				t.Fatalf("containerMissing(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// Apple 1.2/1.3 report an absent container from `container logs` as a
// nested internalError envelope whose notFound cause sits behind the
// "failed to open container logs: " marker. Both the root and the cause
// name the target, so the match stays exact and target-scoped.
func TestAppleLogsMissingAcceptsNestedOpenCauseEnvelope(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "open cause missing id",
			stderr: `Error: internalError: "failed to get logs for container myctr" (cause: "notFound: "failed to open container logs: container with ID myctr not found"")`,
			want:   true,
		},
		{
			name:   "open cause get failed leaf",
			stderr: `Error: internalError: "failed to get logs for container myctr" (cause: "notFound: "failed to open container logs: get failed: container myctr not found"")`,
			want:   true,
		},
		{
			name:   "cause names another container",
			stderr: `Error: internalError: "failed to get logs for container myctr" (cause: "notFound: "failed to open container logs: container with ID other not found"")`,
		},
		{
			name:   "root names another container",
			stderr: `Error: internalError: "failed to get logs for container other" (cause: "notFound: "failed to open container logs: container with ID myctr not found"")`,
		},
		{
			name:   "unrelated application stderr",
			stderr: "Error: application: failed to open container logs: container with ID myctr not found",
		},
		{
			name:   "open cause without a missing id leaf",
			stderr: `Error: internalError: "failed to get logs for container myctr" (cause: "notFound: "failed to open container logs: connection refused"")`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &cli.CLIError{
				Binary: "container", Args: []string{"logs", "-n", "1000", "myctr"},
				ExitCode: 1, Stderr: tc.stderr,
			}
			if got := (appleEngine{}).containerMissing(err); got != tc.want {
				t.Fatalf("containerMissing(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
			if got := wrapNotFoundFor(appleEngine{}, err); errors.Is(got, ErrContainerNotFound) != tc.want {
				t.Fatalf("wrapNotFoundFor(%q) = %v, want ErrContainerNotFound=%v", tc.stderr, got, tc.want)
			}
		})
	}
}

// A caller-chosen name such as "tls" reaches CLIError.Args, so the
// liveness contract reads the CLI's stderr rather than the argv it was
// handed. A real backend outage still reports ErrSystemNotRunning, while a
// genuine TLS or certificate diagnostic still vetoes it.
func TestProbeClassificationIgnoresUserControlledArgv(t *testing.T) {
	for _, target := range []string{"tls", "proxy", "ssh-cert", "config", "certificate"} {
		t.Run(target, func(t *testing.T) {
			original := &cli.CLIError{
				Binary: "container", Args: []string{"logs", "-n", "1000", target},
				ExitCode: 1, Stderr: "XPC connection error: service is not registered",
			}
			if cli.IsProbeConfigurationError(original) {
				t.Fatalf("target %q was read as a configuration diagnostic", target)
			}
			probeErr := &cli.CLIError{
				Binary: "container", Args: []string{"system", "status"},
				ExitCode: 1, Stderr: "XPC connection error",
			}
			got := cli.Classify(
				context.Background(),
				&fixtureProbeRunner{err: probeErr},
				original,
				appleEngine{}.probe(),
			)
			if !errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("Classify() = %v, want ErrSystemNotRunning for target %q", got, target)
			}
			if !errors.Is(got, original) || !errors.Is(got, probeErr) {
				t.Fatalf("Classify() = %v, want original and probe chains", got)
			}
		})
	}
}

func TestProbeClassificationRetainsCertificateVeto(t *testing.T) {
	probeArgs := []string{"version", "--format", "{{.Server.Version}}"}
	original := &cli.CLIError{
		Binary: "docker", Args: probeArgs, ExitCode: 1, Stderr: "command failed",
	}
	probeErr := &cli.CLIError{
		Binary: "docker", Args: probeArgs, ExitCode: 1,
		Stderr: "error during connect: tls: failed to verify certificate: x509: certificate signed by unknown authority",
	}
	got := cli.Classify(
		context.Background(),
		&fixtureProbeRunner{err: probeErr},
		original,
		dockerEngine{}.probe(),
	)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("Classify() = %v, want certificate failure preserved", got)
	}
	// A certificate diagnostic joined by runner instrumentation is still
	// configuration evidence.
	joined := errors.Join(probeErr, errors.New("x509: certificate signed by unknown authority"))
	if !cli.IsProbeConfigurationError(joined) {
		t.Fatal("joined certificate diagnostic was not detected")
	}
}

func TestProductionProbePredicatesInspectJoinedWrappedText(t *testing.T) {
	cases := []struct {
		name      string
		binary    string
		args      []string
		joined    string
		available func(error) bool
	}{
		{
			name:      "docker",
			binary:    "docker",
			args:      []string{"version", "--format", "{{.Server.Version}}"},
			joined:    "Cannot connect to the Docker daemon",
			available: dockerProbeUnavailable,
		},
		{
			name:      "apple",
			binary:    "container",
			args:      []string{"system", "status"},
			joined:    "XPC connection failed",
			available: appleProbeUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cliErr := &cli.CLIError{
				Binary: tc.binary, Args: tc.args, ExitCode: 1,
				Stderr: "probe command failed without backend wording",
			}
			joined := errors.Join(errors.New(tc.joined), cliErr)
			wrapped := fmt.Errorf("instrumented runner: %w", joined)
			if !tc.available(wrapped) {
				t.Fatalf("predicate ignored joined wrapped liveness text %q", tc.joined)
			}

			wrongOperation := &cli.CLIError{
				Binary: tc.binary, Args: []string{"inspect", "myctr"}, ExitCode: 1,
			}
			if tc.available(fmt.Errorf("wrapped: %w", errors.Join(errors.New(tc.joined), wrongOperation))) {
				t.Fatal("predicate accepted joined liveness text from the wrong operation")
			}
		})
	}
}

func TestProductionProbePredicatesKeepJoinedConfigurationAsVeto(t *testing.T) {
	dockerErr := errors.Join(
		&cli.CLIError{
			Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"},
			ExitCode: 1, Stderr: "Cannot connect to the Docker daemon",
		},
		errors.New("x509: certificate signed by unknown authority"),
	)
	if dockerProbeUnavailable(dockerErr) {
		t.Fatal("Docker joined certificate failure was classified as daemon down")
	}

	appleErr := errors.Join(
		&cli.CLIError{
			Binary: "container", Args: []string{"system", "status"},
			ExitCode: 1, Stderr: "XPC connection failed",
		},
		errors.New("proxyconnect tcp: connection refused"),
	)
	if appleProbeUnavailable(appleErr) {
		t.Fatal("Apple joined proxy failure was classified as system down")
	}
}

func TestContainerPrefixNormalizationIsAppleAndOperationScoped(t *testing.T) {
	dockerWorkload := &cli.CLIError{
		Binary: "docker", Args: []string{"exec", "myctr", "sh", "-c", "echo"},
		ExitCode: 7,
		Stderr:   "container: Error response from daemon: No such container: myctr",
	}
	if (dockerEngine{}).containerMissing(dockerWorkload) {
		t.Fatal("Docker workload text spoofed the Apple container prefix")
	}

	appleInspect := &cli.CLIError{
		Binary: "container", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: `container: Error: notFound: "container not found: myctr"`,
	}
	if !(appleEngine{}).containerMissing(appleInspect) {
		t.Fatal("Apple inspect envelope was not accepted through the container prefix")
	}

	appleExecOutput := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "query"}, ExitCode: 7,
		Stderr: `container: Error: notFound: "get failed: container myctr not found"`,
	}
	if (appleEngine{}).containerMissing(appleExecOutput) {
		t.Fatal("Apple exec workload text spoofed the backend container prefix")
	}
}

func TestGenericErrorWrapperIsNotDockerExecEvidence(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker", Args: []string{"exec", "myctr", "true"},
		ExitCode: 7, Stderr: "Error: no such container: myctr",
	}
	if (dockerEngine{}).containerMissing(err) {
		t.Fatal("generic application Error: stderr was treated as Docker exec evidence")
	}

	inspectErr := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"},
		ExitCode: 1, Stderr: "Error: no such object: myctr",
	}
	if !(dockerEngine{}).containerMissing(inspectErr) {
		t.Fatal("Docker inspect's known Error: wrapper was not recognized")
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
