package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestRunFailedCreateHonorsKeepEnv(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error for failed run")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained handle myctr", ctr)
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no failed-create cleanup with CONTAINERGO_KEEP=1", r.deleted)
	}
	if r.callWith("inspect") == nil {
		t.Fatal("failed-create retention did not verify the container")
	}
	if state, stateErr := ctr.State(context.Background()); stateErr != nil || state != StateCreated {
		t.Fatalf("retained State = %q, %v; want created", state, stateErr)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained handle: %v", err)
	}
}

func TestRunCopyFailureHonorsKeepEnv(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner(), fileContent: ""}
	f.imagePresent = true
	f.failPrefix = "cp"
	src := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithFiles(File{HostPath: src, ContainerPath: "/x"}))
	if err == nil {
		t.Fatal("want error when file copy fails")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained handle myctr", ctr)
	}
	if f.callWith("delete") != nil {
		t.Fatal("copy rollback issued delete with CONTAINERGO_KEEP=1")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained handle: %v", err)
	}
}

func TestRunPreCLICopyFailureHonorsKeepEnv(t *testing.T) {
	f := &cpRunner{fakeRunner: newTestRunner()}
	f.imagePresent = true
	t.Setenv("CONTAINERGO_KEEP", "1")

	missing := filepath.Join(t.TempDir(), "not-created")
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithFiles(File{HostPath: missing, ContainerPath: "/x"}))
	if err == nil {
		t.Fatal("want error for missing host path")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained handle myctr", ctr)
	}
	if f.callWith("cp") != nil {
		t.Fatal("pre-CLI copy failure unexpectedly invoked cp")
	}
	if f.callWith("delete") != nil {
		t.Fatal("pre-CLI copy failure issued delete with CONTAINERGO_KEEP=1")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained handle: %v", err)
	}
}

func TestRunWaitFailureHonorsKeepEnv(t *testing.T) {
	f := newTestRunner()
	logs := &stdoutRunner{fakeRunner: f, stdout: "fatal: config invalid\n"}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(logs), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: errors.New("never became ready")}))
	if err == nil {
		t.Fatal("want error when wait strategy fails")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained handle myctr", ctr)
	}
	if f.callWith("delete") != nil {
		t.Fatal("wait rollback issued delete with CONTAINERGO_KEEP=1")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained handle: %v", err)
	}
}

func TestExplicitTerminateIgnoresKeepEnv(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil
	t.Setenv("CONTAINERGO_KEEP", "1")

	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if f.callWith("delete") == nil {
		t.Fatal("explicit Terminate did not delete with CONTAINERGO_KEEP=1")
	}
}

func TestRollbackPreservesDeleteFailure(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "0")
	f := &rollbackDeleteFailureRunner{fakeRunner: newTestRunner()}
	ctr := &Container{
		id:     "myctr",
		uid:    "container-id",
		runner: f,
		eng:    dockerEngine{},
	}
	cause := errors.New("copy failed")

	err := ctr.rollback(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("rollback error = %v, want original cause", err)
	}
	if !strings.Contains(err.Error(), "left behind") {
		t.Fatalf("rollback error = %v, want left-behind detail", err)
	}
}

type rollbackDeleteFailureRunner struct {
	*fakeRunner
}

func (r *rollbackDeleteFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "rm" {
		r.calls = append(r.calls, args)
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "delete denied"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestReuseCopyFailureHonorsKeepEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "rollback", keep: "0", wantDelete: true},
		{name: "keep", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
			base := newTestRunner()
			base.imagePresent = true
			r := &reuseSetupRunner{
				fakeRunner:  base,
				inspectJSON: creationInspectJSON("myctr", "1234567890abcdef"),
			}
			src := filepath.Join(t.TempDir(), "x")
			if err := os.WriteFile(src, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config{
				runner:   r,
				eng:      appleEngine{},
				name:     "myctr",
				creation: "1234567890abcdef",
				files:    []File{{HostPath: src, ContainerPath: "/x"}},
			}

			ctr, err := reuseCreate(context.Background(), "redis:7-alpine", cfg)
			if err == nil {
				t.Fatal("want copy error")
			}
			if got := len(r.deleted) > 0; got != tc.wantDelete {
				t.Fatalf("deleted = %v, want deletion = %t", r.deleted, tc.wantDelete)
			}
			if tc.keep == "1" {
				if ctr == nil || ctr.ID() != "myctr" {
					t.Fatalf("container = %#v, want retained reuse handle", ctr)
				}
				if err := ctr.Terminate(context.Background()); err != nil {
					t.Fatalf("explicit Terminate of retained reuse handle: %v", err)
				}
			} else if ctr != nil {
				t.Fatalf("container = %#v, want nil after default reuse rollback", ctr)
			}
		})
	}
}

type reuseSetupRunner struct {
	*fakeRunner
	inspectJSON string
	deleted     []string
}

func (r *reuseSetupRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		if len(args) > 1 && args[1] == "myctr" {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return []byte(r.inspectJSON), nil, nil
		}
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "copy failed"}
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestRunGeneratedNameRetainedAfterFailedCreate(t *testing.T) {
	r := &generatedFailRunner{
		fakeRunner: newTestRunner(),
		runErr:     &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"},
	}
	r.imagePresent = true
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want failed-create error")
	}
	if ctr == nil || !strings.HasPrefix(ctr.ID(), "containergo-") {
		t.Fatalf("container = %#v, want generated retained name", ctr)
	}
	if ctr.ID() != r.name {
		t.Fatalf("handle ID = %q, run name = %q", ctr.ID(), r.name)
	}
	if r.callWith("delete") != nil {
		t.Fatal("generated failed-create container was automatically deleted")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of generated retained handle: %v", err)
	}
}

type generatedFailRunner struct {
	*fakeRunner
	runErr   error
	name     string
	creation string
}

func (r *generatedFailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--name" {
				r.name = args[i+1]
			}
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				r.creation = value
			}
		}
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		name, creation := r.name, r.creation
		r.mu.Unlock()
		return []byte(creationInspectJSON(name, creation)), nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestReuseFailedCreatePreservesCleanupCLIError(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "0")
	base := newTestRunner()
	base.imagePresent = true
	primary := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "reuse primary failure"}
	cleanup := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "reuse cleanup failure"}
	inner := &failRunRunner{
		fakeRunner:  base,
		runErr:      primary,
		inspectJSON: ownedReuseInspectJSON("myctr"),
		deleteErr:   cleanup,
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: new(int)}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if got := cliErrorWithStderr(err, primary.Stderr); got == nil {
		t.Fatalf("error = %v, want primary reuse CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanup.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup reuse CLIError", err)
	}
}

func TestReuseFailedCreateRetainedHandleHonorsKeepEnv(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	inner := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "reuse start failed"},
		inspectJSON: ownedReuseInspectJSON("myctr"),
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: new(int)}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want reuse create error")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained reuse handle", ctr)
	}
	if len(inner.deleted) != 0 {
		t.Fatalf("deleted = %v, want no automatic reuse cleanup", inner.deleted)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained reuse handle: %v", err)
	}
}

func TestReuseWaitFailureReturnsRetainedHandle(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(f), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: errors.New("never ready")}))
	if err == nil {
		t.Fatal("want reuse wait error")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained reuse handle", ctr)
	}
	if f.callWith("delete") != nil {
		t.Fatal("reuse wait failure issued an automatic delete")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained reuse handle: %v", err)
	}
}

func TestReuseCopyFailureThroughRunReturnsRetainedHandle(t *testing.T) {
	r := &retainedReuseCopyRunner{fakeRunner: newTestRunner()}
	r.imagePresent = true
	src := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(src, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERGO_KEEP", "1")

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: src, ContainerPath: "/input"}))
	if err == nil {
		t.Fatal("want reuse copy error")
	}
	if ctr == nil || ctr.ID() != "myctr" {
		t.Fatalf("container = %#v, want retained reuse handle", ctr)
	}
	if r.callWith("delete") != nil {
		t.Fatal("reuse copy failure issued an automatic delete")
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("explicit Terminate of retained reuse handle: %v", err)
	}
}

type retainedReuseCopyRunner struct {
	*fakeRunner
	created  bool
	creation string
}

func (r *retainedReuseCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.created = true
		for i := 0; i+1 < len(args); i++ {
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				r.creation = value
			}
		}
		r.mu.Unlock()
		return []byte("myctr\n"), nil, nil
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		created, creation := r.created, r.creation
		r.mu.Unlock()
		if !created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
		}
		return []byte(inspectJSONWithStateAndLabels("myctr", "running", "redis:7-alpine", map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: creation,
		})), nil, nil
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "reuse copy failed"}
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}
