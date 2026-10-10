package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestClassifiersRejectPermissionNotFound(t *testing.T) {
	cases := []struct {
		name string
		got  bool
	}{
		{
			name: "apple container",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "permission denied: container not found: myctr",
			}),
		},
		{
			name: "docker container",
			got: (dockerEngine{}).containerMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "permission denied: no such object: myctr",
			}),
		},
		{
			name: "apple image",
			got: (appleEngine{}).imageMissing(&cli.CLIError{
				Binary: "container", Args: []string{"image", "inspect", "redis:7-alpine"},
				ExitCode: 1, Stderr: "permission denied: image not found: redis:7-alpine",
			}),
		},
		{
			name: "docker conflict",
			got: (dockerEngine{}).nameConflict(&cli.CLIError{
				Binary: "docker", Args: []string{"run", "--name", "myctr"},
				ExitCode: 1, Stderr: `permission denied: Conflict. The container name "myctr" is already in use by container abc`,
			}),
		},
		{
			name: "docker configuration",
			got: (dockerEngine{}).containerMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "invalid configuration: no such object: myctr",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got {
				t.Fatal("permission/configuration diagnostic was classified as a backend object result")
			}
		})
	}
}

func TestClassifiersAreBackendCommandAndTargetAware(t *testing.T) {
	cases := []struct {
		name string
		got  bool
	}{
		{
			name: "docker wording through apple",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "Error response from daemon: No such container: myctr",
			}),
		},
		{
			name: "apple wording through docker",
			got: (dockerEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "Error: container not found: myctr",
			}),
		},
		{
			name: "wrong image operation",
			got: (dockerEngine{}).imageMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"pull", "redis:7-alpine"},
				ExitCode: 1, Stderr: "Error response from daemon: No such image: redis:7-alpine",
			}),
		},
		{
			name: "wrong image target",
			got: (dockerEngine{}).imageMissing(&cli.CLIError{
				Binary: "docker", Args: []string{"image", "inspect", "redis:7-alpine"},
				ExitCode: 1, Stderr: "Error response from daemon: No such image: other:tag",
			}),
		},
		{
			name: "wrong conflict operation",
			got: (dockerEngine{}).nameConflict(&cli.CLIError{
				Binary: "docker", Args: []string{"rm", "--force", "myctr"},
				ExitCode: 1, Stderr: `Conflict. The container name "myctr" is already in use by container abc`,
			}),
		},
		{
			name: "wrong container target",
			got: (appleEngine{}).containerMissing(&cli.CLIError{
				Binary: "container", Args: []string{"inspect", "myctr"},
				ExitCode: 1, Stderr: "Error: container not found: other",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got {
				t.Fatal("cross-backend, wrong-command, or wrong-target diagnostic was classified")
			}
		})
	}
}

type reviewTerminateRunner struct {
	err         error
	deleteCalls int
}

func (r *reviewTerminateRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return nil, nil, r.err
	case "system", "version":
		return []byte("running"), nil, nil
	case "delete", "rm":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestTerminateDoesNotTreatPermissionNotFoundAsVerifiedAbsence(t *testing.T) {
	original := &cli.CLIError{
		Binary: "container", Args: []string{"inspect", "myctr"},
		ExitCode: 1, Stderr: "permission denied: container not found: myctr",
	}
	r := &reviewTerminateRunner{err: original}
	ctr := &Container{id: "myctr", runner: r, eng: appleEngine{}, creation: "generation"}

	if err := ctr.Terminate(context.Background()); err == nil {
		t.Fatal("Terminate returned success for an unverified permission error")
	}
	if r.deleteCalls != 0 {
		t.Fatalf("delete calls = %d, want 0", r.deleteCalls)
	}
}

func TestCreateRaceMissingIsAnchoredAndCommandAwareReview(t *testing.T) {
	cases := []struct {
		name string
		err  *cli.CLIError
		want bool
	}{
		{
			name: "direct race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with ID myctr not found",
			},
			want: true,
		},
		{
			name: "application wording",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: application: container not found: myctr",
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
			name: "wrong backend",
			err: &cli.CLIError{
				Binary: "docker", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with ID myctr not found",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createRaceMissing(tc.err); got != tc.want {
				t.Fatalf("createRaceMissing() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestDockerProbeAvailabilityIgnoresAuthWordsInEndpointPath(t *testing.T) {
	probeArgs := []string{"version", "--format", "{{.Server.Version}}"}
	cases := []struct {
		name   string
		stderr string
	}{
		{
			name:   "credential path",
			stderr: "Cannot connect to the Docker daemon at unix:///Users/test/.docker/credential/docker.sock: connect: connection refused",
		},
		{
			name:   "forbidden path",
			stderr: "Cannot connect to the Docker daemon at tcp://forbidden.example:2376: connect: connection refused",
		},
		{
			name:   "unauthorized path",
			stderr: "Cannot connect to the Docker daemon at ssh://unauthorized@example: connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &cli.CLIError{Binary: "docker", Args: probeArgs, ExitCode: 1, Stderr: tc.stderr}
			if !dockerProbeUnavailable(err) {
				t.Fatalf("dockerProbeUnavailable(%q) = false, want daemon-down evidence", tc.stderr)
			}
		})
	}
}

func TestDockerProbeRejectsPreciseConfigurationAndAuth(t *testing.T) {
	probeArgs := []string{"version", "--format", "{{.Server.Version}}"}
	cases := []string{
		"error during connect: invalid configuration for current context",
		"error during connect: authentication required",
		"error during connect: 403 forbidden",
		"error during connect: credential helper failed",
	}
	for _, stderr := range cases {
		t.Run(stderr, func(t *testing.T) {
			err := &cli.CLIError{Binary: "docker", Args: probeArgs, ExitCode: 1, Stderr: stderr}
			if dockerProbeUnavailable(err) {
				t.Fatalf("dockerProbeUnavailable(%q) = true, want false", stderr)
			}
		})
	}
}

func TestWaitForExecKeepsTimeoutTextAsExitResult(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr: &cli.CLIError{
			Binary: "container", Args: []string{"exec", "myctr", "command timed out"},
			ExitCode: 7, Stderr: "i/o timeout",
		},
	}
	ctr := runTestContainer(t, f)
	strategy := wait.ForExec([]string{"query"}).
		WithExitCodeMatcher(func(code int) bool { return code == 7 }).
		WithStartupTimeout(200 * time.Millisecond).
		WithPollInterval(10 * time.Millisecond)

	if err := strategy.WaitUntilReady(context.Background(), waitTarget{c: ctr}); err != nil {
		t.Fatalf("ForExec: %v, want accepted exit code 7", err)
	}
}

type reviewProbeRunner struct {
	calls int
}

func (r *reviewProbeRunner) Run(_ context.Context, _ ...string) ([]byte, []byte, error) {
	r.calls++
	return nil, nil, errors.New("unexpected probe")
}

func TestClassifyContextJoinsNonCLILaunchError(t *testing.T) {
	original := errors.New("backend launch failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &reviewProbeRunner{}
	got := cli.Classify(ctx, runner, original, cli.Probe{Args: []string{"version"}, Hint: "start the daemon"})
	if !errors.Is(got, original) {
		t.Fatalf("error = %v, want original launch error", got)
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", got)
	}
	if runner.calls != 0 {
		t.Fatalf("probe calls = %d, want 0", runner.calls)
	}
}
