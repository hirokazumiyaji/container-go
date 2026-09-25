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
	// State always performs a fresh inspect, even with identity cached.
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
