package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type appleErrorFixture struct {
	Name             string `json:"name"`
	Operation        string `json:"operation"`
	Target           string `json:"target"`
	Stderr           string `json:"stderr"`
	ContainerMissing bool   `json:"containerMissing"`
}

func TestAppleContainerMissingFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/cli_stderr_apple_1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []appleErrorFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("fixture file is empty")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			args := []string{fixture.Operation, fixture.Target}
			if fixture.Operation == "exec" {
				args = append(args, "true")
			}
			cliErr := &cli.CLIError{
				Binary: "container",
				Args:   args,
				Stderr: fixture.Stderr,
			}
			if got := (appleEngine{}).containerMissing(cliErr); got != fixture.ContainerMissing {
				t.Fatalf("containerMissing(%q) = %v, want %v", fixture.Stderr, got, fixture.ContainerMissing)
			}
			if got := isNotFound(cliErr); got != fixture.ContainerMissing {
				t.Fatalf("isNotFound(%q) = %v, want %v", fixture.Stderr, got, fixture.ContainerMissing)
			}
		})
	}
}

type appleLifecycleErrorRunner struct {
	*fakeRunner
	inspectErr   error
	operation    string
	operationErr error
	inspectJSON  string
}

func (r *appleLifecycleErrorRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" && r.inspectErr != nil {
		return nil, nil, r.inspectErr
	}
	if args[0] == r.operation && r.operationErr != nil {
		return nil, nil, r.operationErr
	}
	if args[0] == "inspect" && r.inspectJSON != "" {
		return []byte(r.inspectJSON), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func typedAppleMissing(operation string) *cli.CLIError {
	args := []string{operation, "myctr"}
	if operation == "exec" {
		args = append(args, "true")
	}
	return &cli.CLIError{
		Binary: "container",
		Args:   args,
		Stderr: `Error: internalError: "operation failed" (cause: "notFound: \"container not found: myctr\"")`,
	}
}

func TestAppleTypedNotFoundLifecycleSemantics(t *testing.T) {
	t.Run("State", func(t *testing.T) {
		ctr := runTestContainer(t, newTestRunner())
		ctr.info = nil
		ctr.runner = &appleLifecycleErrorRunner{
			fakeRunner: newTestRunner(),
			operation:  "inspect",
			inspectErr: typedAppleMissing("inspect"),
		}
		if _, err := ctr.State(context.Background()); !errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("State error = %v, want ErrContainerNotFound", err)
		}
	})

	t.Run("Logs", func(t *testing.T) {
		ctr := runTestContainer(t, newTestRunner())
		ctr.runner = &appleLifecycleErrorRunner{
			fakeRunner:   newTestRunner(),
			operation:    "logs",
			operationErr: typedAppleMissing("logs"),
		}
		if _, err := ctr.Logs(context.Background()); !errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("Logs error = %v, want ErrContainerNotFound", err)
		}
	})

	t.Run("Exec", func(t *testing.T) {
		ctr := runTestContainer(t, newTestRunner())
		ctr.runner = &appleLifecycleErrorRunner{
			fakeRunner:   newTestRunner(),
			operation:    "exec",
			operationErr: typedAppleMissing("exec"),
			inspectErr:   typedAppleMissing("inspect"),
		}
		if _, _, err := ctr.Exec(context.Background(), []string{"true"}); !errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("Exec error = %v, want ErrContainerNotFound", err)
		}
	})

	t.Run("Terminate", func(t *testing.T) {
		ctr := runTestContainer(t, newTestRunner())
		ctr.info = nil
		ctr.runner = &appleLifecycleErrorRunner{
			fakeRunner:   newTestRunner(),
			operation:    "delete",
			operationErr: typedAppleMissing("delete"),
			inspectJSON:  creationInspectJSON("myctr", ctr.creation),
		}
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Fatalf("Terminate error = %v, want nil for a missing container", err)
		}
	})
}
