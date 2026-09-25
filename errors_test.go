package container

import (
	"context"
	"errors"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestInspectFreshWrapsErrContainerNotFound(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &inspectNotFoundRunner{
		err: &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: `No such object: myctr`},
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
		err: &cli.CLIError{Args: []string{"exec", "myctr"}, ExitCode: 1, Stderr: `No such container: myctr`},
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

func TestCLIErrorAliasUsableWithErrorsAs(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &execNotFoundRunner{
		err: &cli.CLIError{Args: []string{"exec", "myctr"}, ExitCode: 1, Stderr: `No such container: myctr`},
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

func cliErrorWithStderr(err error, stderr string) *CLIError {
	if err == nil {
		return nil
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) && cliErr.Stderr == stderr {
		return cliErr
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			if found := cliErrorWithStderr(child, stderr); found != nil {
				return found
			}
		}
	case interface{ Unwrap() error }:
		return cliErrorWithStderr(e.Unwrap(), stderr)
	}
	return nil
}

func TestCleanupErrorExposesBothErrors(t *testing.T) {
	operationErr := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "operation failed"}
	cleanupErr := &CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: "cleanup failed"}
	err := withCleanupError(operationErr, cleanupErr)

	var joined *CleanupError
	if !errors.As(err, &joined) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if got := cliErrorWithStderr(err, operationErr.Stderr); got != operationErr {
		t.Fatalf("operation CLIError = %v, want %v", got, operationErr)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got != cleanupErr {
		t.Fatalf("cleanup CLIError = %v, want %v", got, cleanupErr)
	}
}

func TestLogsWrapsErrContainerNotFound(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &execNotFoundRunner{
		err: &cli.CLIError{Args: []string{"logs", "myctr"}, ExitCode: 1, Stderr: `No such container: myctr`},
	}
	if _, err := ctr.Logs(context.Background()); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Logs error = %v, want ErrContainerNotFound", err)
	}
}

func TestContainerNotFoundClassificationIsExact(t *testing.T) {
	positive := []*cli.CLIError{
		{Binary: "docker", Args: []string{"inspect", "myctr"}, Stderr: "Error: No such object: myctr"},
		{Binary: "docker", Args: []string{"rm", "--force", "myctr"}, Stderr: "Error response from daemon: No such container: myctr"},
		{Binary: "container", Args: []string{"inspect", "myctr"}, Stderr: "container not found: myctr"},
		{Binary: "container", Args: []string{"exec", "myctr", "true"}, Stderr: "get failed: container myctr not found"},
		{Binary: "container", Args: []string{"delete", "--force", "myctr"}, Stderr: "failed to delete container: container with ID myctr not found"},
		{Binary: "container", Args: []string{"logs", "myctr"}, Stderr: "failed to get logs for container myctr: failed to open container logs: container with ID myctr not found"},
	}
	for _, err := range positive {
		if !isNotFound(err) || !errors.Is(wrapNotFound(err), ErrContainerNotFound) {
			t.Errorf("isNotFound(%v) = false, want structured absence", err)
		}
	}

	negative := []*cli.CLIError{
		{Binary: "container", Args: []string{"inspect", "myctr"}, Stderr: "permission denied: not found"},
		{Binary: "container", Args: []string{"run"}, Stderr: "application error: container not found: myctr"},
		{Binary: "container", Args: []string{"inspect", "myctr"}, Stderr: "container not found: other"},
		{Binary: "docker", Args: []string{"inspect", "myctr"}, Stderr: "configuration error: no such object: myctr"},
	}
	for _, err := range negative {
		if isNotFound(err) || errors.Is(wrapNotFound(err), ErrContainerNotFound) {
			t.Errorf("isNotFound(%v) = true, want permission/config/application error", err)
		}
	}
}

type emptyInspectRunner struct{}

func (emptyInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return []byte(`[]`), nil, nil
	case "system", "version", "info":
		return []byte("running"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestEmptyInspectIsContainerNotFound(t *testing.T) {
	ctr := &Container{
		id:       "missing",
		runner:   emptyInspectRunner{},
		eng:      appleEngine{},
		creation: "0123456789abcdef",
	}
	if _, err := ctr.State(context.Background()); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("State error = %v, want ErrContainerNotFound", err)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate empty inspect = %v, want idempotent nil", err)
	}
}
