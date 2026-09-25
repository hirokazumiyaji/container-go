package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestKeepRetainsOwnedReuseCreateBeforeConflictRetry(t *testing.T) {
	cases := []struct {
		name string
		err  *cli.CLIError
	}{
		{
			name: "name conflict",
			err: &cli.CLIError{
				Args: []string{"run"}, ExitCode: 1,
				Stderr: `Error: already exists: container "myctr"`,
			},
		},
		{
			name: "create race",
			err: &cli.CLIError{
				Args: []string{"run"}, ExitCode: 1,
				Stderr: `Error: container with ID myctr not found`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := newContainerName()
			base := newTestRunner()
			base.imagePresent = true
			inner := &failRunRunner{
				fakeRunner:  base,
				runErr:      tc.err,
				inspectJSON: strings.Replace(ownedReuseInspectJSON(name), `"state": "created"`, `"state": "running"`, 1),
			}
			wrapper := &reuseFailWrapper{failRunRunner: inner, calls: new(int)}
			t.Setenv("CONTAINERGO_KEEP", "1")

			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName(name), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
			if err == nil {
				t.Fatal("Run succeeded; want the original create error")
			}
			if ctr == nil || ctr.ID() != name {
				t.Fatalf("container = %#v, want owned retained handle", ctr)
			}
			if got := *wrapper.calls; got != 2 {
				t.Fatalf("inspect calls before return = %d, want initial lookup plus ownership check", got)
			}
			if got := cliErrorWithStderr(err, tc.err.Stderr); got == nil {
				t.Fatalf("error = %v, want original create error", err)
			}
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Fatalf("terminate retained handle: %v", err)
			}
		})
	}
}

func TestKeepDoesNotClaimUnverifiedReusePeerBeforeConflictRetry(t *testing.T) {
	name := newContainerName()
	base := newTestRunner()
	base.imagePresent = true
	inner := &conflictThenAttachRunner{fakeRunner: base}
	runner := &countingConflictRunner{conflictThenAttachRunner: inner}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr == nil || ctr.ID() != name {
		t.Fatalf("container = %#v, want attached peer handle", ctr)
	}
	if got := runner.inspectCalls; got != 3 {
		t.Fatalf("inspect calls = %d, want initial lookup, ownership check, then peer attach", got)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate attached peer: %v", err)
	}
}

type countingConflictRunner struct {
	*conflictThenAttachRunner
	inspectCalls int
}

func (r *countingConflictRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.inspectCalls++
	}
	return r.conflictThenAttachRunner.Run(ctx, args...)
}

func TestKeepDoesNotRetryWhenRetainedLookupFails(t *testing.T) {
	name := newContainerName()
	base := newTestRunner()
	base.imagePresent = true
	inner := &failRunRunner{
		fakeRunner: base,
		runErr: &cli.CLIError{
			Args: []string{"run"}, ExitCode: 1,
			Stderr: `Error: already exists: container "myctr"`,
		},
		inspectErr: &cli.CLIError{
			Args: []string{"inspect"}, ExitCode: 1,
			Stderr: "permission denied",
		},
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: new(int)}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded; want retained lookup error")
	}
	if ctr != nil {
		t.Fatalf("container = %#v, want no unverified handle", ctr)
	}
	if got := *wrapper.calls; got != 2 {
		t.Fatalf("inspect calls = %d, want initial lookup and one failed ownership check", got)
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
}

func TestKeepRetainsOwnedFailedCreateOnNameConflict(t *testing.T) {
	name := newContainerName()
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{
		Args: []string{"run"}, ExitCode: 1,
		Stderr: `Error: already exists: container "myctr"`,
	}
	runner := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedInspectJSON(name),
	}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), withRunner(runner), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded; want name conflict")
	}
	if ctr == nil || ctr.ID() != name {
		t.Fatalf("container = %#v, want owned retained handle", ctr)
	}
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original name conflict", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want conflict diagnostic", err)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate retained handle: %v", err)
	}
}

func TestKeepDoesNotClaimForeignFailedCreateOnNameConflict(t *testing.T) {
	name := newContainerName()
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{
		Args: []string{"run"}, ExitCode: 1,
		Stderr: `Error: already exists: container "myctr"`,
	}
	runner := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: foreignInspectJSON(name),
	}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName(name), withRunner(runner), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run succeeded; want name conflict")
	}
	if ctr != nil {
		t.Fatalf("container = %#v, want no handle for foreign peer", ctr)
	}
	if !errors.Is(err, runErr) && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want name conflict", err)
	}
}
