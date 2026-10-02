package container

import (
	"context"
	"errors"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestErrPortNotExposedDescribesAllFailureCases(t *testing.T) {
	const want = "port is not declared or has no usable host binding"
	if got := ErrPortNotExposed.Error(); got != want {
		t.Fatalf("ErrPortNotExposed = %q, want %q", got, want)
	}
}

func TestDockerNetworkErrorsAreDiscriminable(t *testing.T) {
	if err := dockerNetworkModeError("bridge", "host", nil); !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("mode error = %v, want ErrNetworkMismatch", err)
	}
	if err := dockerNetworkEndpointError("none"); !errors.Is(err, ErrPortNotExposed) || !errors.Is(err, ErrNoReachableHost) {
		t.Fatalf("none endpoint error = %v, want port and host sentinels", err)
	}
}

func TestInspectFreshWrapsErrContainerNotFound(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &inspectNotFoundRunner{
		err: &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: `Error: container not found: "myctr"`},
	}
	// Clear cached info so inspectFresh runs.
	ctr.info = nil
	if _, err := ctr.State(context.Background()); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want ErrContainerNotFound", err)
	}
}

func TestExecWrapsErrContainerNotFound(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &execNotFoundRunner{
		err: &cli.CLIError{Args: []string{"exec", "myctr"}, ExitCode: 1, Stderr: `Error: get failed: container myctr not found`},
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, want ErrContainerNotFound", err)
	}
}

type inspectNotFoundRunner struct {
	err error
}

func (n *inspectNotFoundRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "system" && args[1] == "status" {
		return []byte("running"), nil, nil
	}
	if len(args) > 0 && (args[0] == "inspect" || args[0] == "exec" || args[0] == "logs") {
		return nil, nil, n.err
	}
	return nil, nil, nil
}

type execNotFoundRunner struct {
	err error
}

func (n *execNotFoundRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "system" && args[1] == "status" {
		return []byte("running"), nil, nil
	}
	if len(args) > 0 && args[0] == "version" {
		return []byte("29.7"), nil, nil
	}
	return nil, nil, n.err
}

func TestCleanupErrorUnwrapsOperationAndCleanup(t *testing.T) {
	operationErr := errors.New("operation failed")
	cleanupErr := errors.New("cleanup failed")
	err := withCleanupError(operationErr, &CleanupError{Container: "myctr", Err: cleanupErr})
	if !errors.Is(err, operationErr) {
		t.Fatalf("errors.Is(operation) = false for %v", err)
	}
	var wrapped *CleanupError
	if !errors.As(err, &wrapped) || wrapped.Container != "myctr" || wrapped.Err != cleanupErr {
		t.Fatalf("errors.As(CleanupError) = %#v, want Container=myctr Err=%v", wrapped, cleanupErr)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("errors.Is(cleanup) = false for %v", err)
	}
}

func TestCLIErrorAliasUsableWithErrorsAs(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &execNotFoundRunner{
		err: &cli.CLIError{Args: []string{"exec", "myctr"}, ExitCode: 1, Stderr: `Error: get failed: container myctr not found`},
	}
	_, _, err := ctr.Exec(context.Background(), []string{"true"})
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want errors.As(*CLIError)", err)
	}
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want errors.Is(ErrContainerNotFound)", err)
	}
}

func TestAppleInspectTargetedNotFound(t *testing.T) {
	err := &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: `Error: container not found: "myctr"`}
	if !isNotFoundFor(appleEngine{}, err) {
		t.Fatal("targeted Apple inspect not-found was not recognized")
	}
}

func TestCreateRaceMissingIsAnchoredAndCommandAware(t *testing.T) {
	cases := []struct {
		name string
		err  *cli.CLIError
		want bool
	}{
		{
			name: "direct Apple race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with ID myctr not found",
			},
			want: true,
		},
		{
			name: "wrapped Apple race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: failed to bootstrap container: container with ID myctr not found",
			},
			want: true,
		},
		{
			name: "typed Apple race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: `Error: notFound: "container with id myctr not found"`,
			},
			want: true,
		},
		{
			name: "nested typed Apple race",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: `Error: internalError: "failed to run container" (cause: "notFound: \"container with id myctr not found\"")`,
			},
			want: true,
		},
		{
			name: "generic application message",
			err: &cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: application: container not found: myctr",
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

func TestApplicationNotFoundOutputIsNotContainerMissing(t *testing.T) {
	appleErr := &cli.CLIError{
		Args:   []string{"exec", "myctr", "true"},
		Stderr: "Error: application: container not found: myctr",
	}
	if isNotFoundFor(appleEngine{}, appleErr) {
		t.Fatal("Apple application output was treated as a missing container")
	}
	dockerErr := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"rm", "--force", "myctr"},
		Stderr: "Error: application: no such container: myctr",
	}
	if isNotFoundFor(dockerEngine{}, dockerErr) {
		t.Fatal("Docker application output was treated as a missing container")
	}
}

func TestDockerVolumeNotFoundIsNotContainerMissing(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"rm", "--force", "--volumes", "container-id"},
		Stderr: "error removing volume: volume driver plugin not found",
	}
	if (dockerEngine{}).containerMissing(err) {
		t.Fatal("volume/plugin not-found error was classified as a missing container")
	}
	if isNotFoundFor(dockerEngine{}, err) {
		t.Fatal("volume/plugin not-found error was treated as idempotent")
	}
}

func TestDockerVolumeErrorMentioningContainerIsNotContainerMissing(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"rm", "--force", "--volumes", "container-id"},
		Stderr: "error removing volume data for container container-id: volume driver not found",
	}
	if isNotFoundFor(dockerEngine{}, err) {
		t.Fatal("volume error mentioning the container was treated as a missing container")
	}
}

func TestDockerVolumeInspectNotFoundIsNotContainerMissing(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"volume", "inspect", "named-volume"},
		Stderr: "no such volume: named-volume",
	}
	if isNotFoundFor(dockerEngine{}, err) {
		t.Fatal("volume inspect not-found was treated as a missing container")
	}
}

func TestDockerMatcherRequiresDockerBinary(t *testing.T) {
	err := &cli.CLIError{Args: []string{"inspect", "myctr"}, Stderr: "Error: No such object: myctr"}
	if (dockerEngine{}).containerMissing(err) {
		t.Fatal("empty binary was accepted as a Docker error")
	}
}

func TestDockerMissingContainerTargetIsExact(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"rm", "--force", "abc"},
		Stderr: "Error response from daemon: No such container: abcd",
	}
	if isNotFoundFor(dockerEngine{}, err) {
		t.Fatal("a different container ID prefix was treated as the requested target")
	}
}

func TestDockerPreciseDeleteNotFoundIsContainerMissing(t *testing.T) {
	err := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"rm", "--force", "--volumes", "container-id"},
		Stderr: "Error response from daemon: No such container: container-id",
	}
	if !isNotFoundFor(dockerEngine{}, err) {
		t.Fatal("precise Docker rm not-found was not recognized")
	}
}

func TestLogsWrapsErrContainerNotFound(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &execNotFoundRunner{
		err: &cli.CLIError{Args: []string{"logs", "myctr"}, ExitCode: 1, Stderr: `Error: failed to get logs for container myctr: failed to open container logs: container with ID myctr not found`},
	}
	if _, err := ctr.Logs(context.Background()); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Logs error = %v, want ErrContainerNotFound", err)
	}
}
