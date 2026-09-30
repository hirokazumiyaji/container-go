package container

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type contextRaceRunner struct {
	stdout []byte
	err    error
	cancel context.CancelFunc
}

func (r *contextRaceRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return r.stdout, nil, r.err
}

func TestImageExistsContextVetoesMissing(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			cliErr := &cli.CLIError{
				Binary: "docker", Args: []string{"image", "inspect", "redis:7-alpine"},
				ExitCode: 1, Stderr: "Error response from daemon: No such image: redis:7-alpine",
			}
			runner := &contextRaceRunner{err: errors.Join(cliErr, cause)}
			exists, err := imageExists(context.Background(), runner, dockerEngine{}, "redis:7-alpine", "")
			if exists || !errors.Is(err, cause) {
				t.Fatalf("imageExists = %v, %v; want false and %v", exists, err, cause)
			}
		})
	}
}

func TestImageExistsChecksContextAtTerminalReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &contextRaceRunner{stdout: []byte("[]"), cancel: cancel}
	exists, err := imageExists(ctx, runner, dockerEngine{}, "redis:7-alpine", "")
	if exists || !errors.Is(err, context.Canceled) {
		t.Fatalf("imageExists = %v, %v; want false and context cancellation", exists, err)
	}
}

func TestDeleteContextVetoesNotFoundSuccess(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			cliErr := &cli.CLIError{
				Binary: "container", Args: []string{"delete", "--force", "myctr"},
				ExitCode: 1, Stderr: "Error: failed to delete container: container with ID myctr not found",
			}
			runner := &contextRaceRunner{err: errors.Join(cliErr, cause)}
			ctr := &Container{id: "myctr", uid: "immutable", runner: runner, eng: appleEngine{}}
			err := ctr.delete(context.Background(), "immutable")
			if !errors.Is(err, cause) {
				t.Fatalf("delete error = %v, want %v", err, cause)
			}
		})
	}
}

func TestTerminateInspectContextVetoesNotFoundSuccess(t *testing.T) {
	cliErr := &cli.CLIError{
		Binary: "container", Args: []string{"inspect", "myctr"},
		ExitCode: 1, Stderr: "Error: container not found: myctr",
	}
	runner := &contextRaceRunner{err: errors.Join(cliErr, context.Canceled)}
	ctr := &Container{id: "myctr", creation: "generation", runner: runner, eng: appleEngine{}}
	if err := ctr.Terminate(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Terminate error = %v, want context cancellation", err)
	}
}

func TestDeleteChecksContextAtTerminalReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &contextRaceRunner{cancel: cancel}
	ctr := &Container{id: "myctr", uid: "immutable", runner: runner, eng: appleEngine{}}
	if err := ctr.delete(ctx, "immutable"); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete error = %v, want context cancellation", err)
	}
}

type pruneContextRunner struct {
	cause  error
	cancel context.CancelFunc
}

func (r *pruneContextRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ls" {
		return []byte("id\n"), nil, nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.cause == nil {
		return nil, nil, nil
	}
	cliErr := &cli.CLIError{
		Binary: "container", Args: args, ExitCode: 1,
		Stderr: "Error: failed to delete container: container with ID id not found",
	}
	return nil, nil, errors.Join(cliErr, r.cause)
}

func parsePruneIDs(data []byte) ([]string, error) {
	return splitNonEmptyLines(data), nil
}

func TestPruneContextVetoesRemovedIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *pruneContextRunner
		want   error
	}{
		{name: "joined cancellation", runner: &pruneContextRunner{cause: context.Canceled}, want: context.Canceled},
		{name: "joined deadline", runner: &pruneContextRunner{cause: context.DeadlineExceeded}, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removed, err := pruneListed(context.Background(), tc.runner, appleEngine{}, []string{"ls"}, parsePruneIDs, "prune")
			if len(removed) != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("prune removed/error = %v, %v; want no IDs and %v", removed, err, tc.want)
			}
		})
	}
}

func TestPruneChecksContextAtTerminalReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &pruneContextRunner{cancel: cancel}
	removed, err := pruneListed(ctx, runner, appleEngine{}, []string{"ls"}, parsePruneIDs, "prune")
	if len(removed) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("prune removed/error = %v, %v; want no IDs and context cancellation", removed, err)
	}
}

type detachedReuseContextRunner struct {
	*fakeRunner
	cause    error
	runCalls int
}

func (r *detachedReuseContextRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		return nil, nil, &cli.CLIError{
			Binary: "container", Args: args, ExitCode: 1,
			Stderr: "Error: container not found: myctr",
		}
	case "run":
		r.runCalls++
		conflict := &cli.CLIError{
			Binary: "container", Args: args, ExitCode: 1,
			Stderr: "Error: container with id myctr already exists",
		}
		return nil, nil, errors.Join(conflict, r.cause)
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestDetachedReuseCreateContextVetoesConflictRetry(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &detachedReuseContextRunner{fakeRunner: base, cause: context.DeadlineExceeded}
	cfg := newConfig()
	cfg.runner = runner
	cfg.eng = appleEngine{}
	cfg.name = "myctr"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ctr, err := reuseEnsureContainer(ctx, "redis:7-alpine", cfg)
	if ctr != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reuse result = %v, %v; want no container and context deadline", ctr, err)
	}
	if runner.runCalls != 1 {
		t.Fatalf("run calls = %d, want one non-retried create", runner.runCalls)
	}
}

type noProbeRunner struct {
	calls int
}

func (r *noProbeRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	r.calls++
	return nil, nil, nil
}

func TestClassifyAmbiguityPreservesTerminalContext(t *testing.T) {
	orig := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"}, ExitCode: 1,
		Stderr: "Error: application: container not found: myctr",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &noProbeRunner{}
	got := classifyError(ctx, runner, orig, appleEngine{})
	if !errors.Is(got, orig) || !errors.Is(got, context.Canceled) {
		t.Fatalf("classified error = %v, want original and context cancellation", got)
	}
	if runner.calls != 0 {
		t.Fatalf("probe calls = %d, want none", runner.calls)
	}
}

func TestExecChecksContextAtSuccessfulTerminalReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &contextRaceRunner{stdout: []byte("application output"), cancel: cancel}
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}
	code, _, err := ctr.Exec(ctx, []string{"query"})
	if code != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec result = %d, %v; want context cancellation error", code, err)
	}
}

type cancelAfterStreamRunner struct {
	noProbeRunner
	cancel context.CancelFunc
}

func (r *cancelAfterStreamRunner) Stream(context.Context, ...string) (io.ReadCloser, error) {
	r.cancel()
	return nil, nil
}

func TestFollowLogsChecksContextAtSuccessfulTerminalReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctr := &Container{
		id: "myctr", runner: &cancelAfterStreamRunner{cancel: cancel}, eng: appleEngine{},
	}
	stream, err := ctr.FollowLogs(ctx)
	if stream != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("FollowLogs stream/error = %v, %v; want no stream and context cancellation", stream, err)
	}
}

func TestContextErrorVetoesBackendMatchers(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			conflict := errors.Join(&cli.CLIError{
				Binary: "container", Args: []string{"run", "--name", "myctr"},
				Stderr: "Error: container with id myctr already exists",
			}, cause)
			missing := errors.Join(&cli.CLIError{
				Binary: "container", Args: []string{"delete", "--force", "myctr"},
				Stderr: "Error: failed to delete container: container with ID myctr not found",
			}, cause)
			imageMissing := errors.Join(&cli.CLIError{
				Binary: "container", Args: []string{"image", "inspect", "redis:7-alpine"},
				Stderr: "Error: image not found: redis:7-alpine",
			}, cause)
			if (appleEngine{}).nameConflict(conflict) || (appleEngine{}).containerMissing(missing) || (appleEngine{}).imageMissing(imageMissing) {
				t.Fatal("joined context did not veto backend matcher")
			}
			confirmedMissing := errors.Join(&inspectTargetNotFoundError{id: "myctr"}, cause)
			if isNotFound(confirmedMissing) || isNotFoundFor(appleEngine{}, confirmedMissing) || errors.Is(confirmedMissing, ErrContainerNotFound) {
				t.Fatal("joined context did not veto an already-confirmed absence")
			}
		})
	}
}

func TestBackendMatchersRequireParsedTarget(t *testing.T) {
	cases := []struct {
		name string
		got  bool
	}{
		{name: "apple image", got: (appleEngine{}).imageMissing(&cli.CLIError{
			Binary: "container", Args: []string{"image", "inspect"},
			Stderr: "Error: image not found: redis:7-alpine",
		})},
		{name: "docker image", got: (dockerEngine{}).imageMissing(&cli.CLIError{
			Binary: "docker", Args: []string{"image", "inspect"},
			Stderr: "Error response from daemon: No such image: redis:7-alpine",
		})},
		{name: "apple container", got: (appleEngine{}).containerMissing(&cli.CLIError{
			Binary: "container", Args: []string{"inspect"},
			Stderr: "Error: container not found: myctr",
		})},
		{name: "docker container", got: (dockerEngine{}).containerMissing(&cli.CLIError{
			Binary: "docker", Args: []string{"inspect"},
			Stderr: "Error: no such object: myctr",
		})},
		{name: "apple conflict", got: (appleEngine{}).nameConflict(&cli.CLIError{
			Binary: "container", Args: []string{"run", "redis:7-alpine"},
			Stderr: "Error: container with id myctr already exists",
		})},
		{name: "docker conflict", got: (dockerEngine{}).nameConflict(&cli.CLIError{
			Binary: "docker", Args: []string{"run", "redis:7-alpine"},
			Stderr: `Conflict. The container name "/myctr" is already in use by container abc`,
		})},
		{name: "create race", got: createRaceMissing(&cli.CLIError{
			Binary: "container", Args: []string{"run", "redis:7-alpine"},
			Stderr: "Error: container with ID myctr not found",
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got {
				t.Fatal("matcher accepted an absence/conflict without a parsed target")
			}
		})
	}
}

func TestInspectEmptyOutputWrapsContainerNotFound(t *testing.T) {
	for _, eng := range []engine{appleEngine{}, dockerEngine{}} {
		t.Run(eng.name(), func(t *testing.T) {
			runner := &contextRaceRunner{stdout: []byte("[]")}
			ctr := &Container{id: "myctr", runner: runner, eng: eng}
			_, err := ctr.State(context.Background())
			if !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("State error = %v, want ErrContainerNotFound", err)
			}
			var cliErr *CLIError
			if errors.As(err, &cliErr) {
				t.Fatalf("State error unexpectedly exposed CLIError: %v", cliErr)
			}
		})
	}
}

func TestEmptyInspectPreservesBackendErrors(t *testing.T) {
	cases := []engine{appleEngine{}, dockerEngine{}}
	for _, eng := range cases {
		t.Run(eng.name(), func(t *testing.T) {
			runner := &contextRaceRunner{stdout: []byte("not json")}
			ctr := &Container{id: "myctr", runner: runner, eng: eng}
			_, err := ctr.State(context.Background())
			if err == nil || errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("State error = %v, want parse error", err)
			}
		})
	}
}
