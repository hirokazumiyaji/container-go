package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type backendStderrFixture struct {
	Name             string `json:"name"`
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

// The fixture files contain the verified stderr shapes from the supported
// CLI versions; the table keeps each classifier's positive and negative
// contract together.
func TestBackendStderrFixtures(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		classifier backendStderrClassifier
	}{
		{
			name: "apple",
			file: "cli_stderr_apple_1.3.0.json",
			classifier: backendStderrClassifier{
				nameConflict:     appleEngine{}.nameConflict,
				imageMissing:     appleEngine{}.imageMissing,
				containerMissing: appleEngine{}.containerMissing,
			},
		},
		{
			name: "docker",
			file: "cli_stderr_docker_29.json",
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
					err := &cli.CLIError{
						Binary:   tc.name,
						Args:     []string{"fixture"},
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
		for _, stderr := range messages {
			err := &cli.CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: stderr}
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
			Args:     args,
			ExitCode: 1,
			Stderr:   `Error: container not found: "ambiguous-reuse"`,
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
