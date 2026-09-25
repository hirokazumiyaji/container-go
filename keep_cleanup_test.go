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

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error for failed run")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no failed-create cleanup with CONTAINERGO_KEEP=1", r.deleted)
	}
	if r.callWith("inspect") != nil {
		t.Fatal("failed-create cleanup inspected the container with CONTAINERGO_KEEP=1")
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

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithFiles(File{HostPath: src, ContainerPath: "/x"}))
	if err == nil {
		t.Fatal("want error when file copy fails")
	}
	if f.callWith("delete") != nil {
		t.Fatal("copy rollback issued delete with CONTAINERGO_KEEP=1")
	}
}

func TestRunWaitFailureHonorsKeepEnv(t *testing.T) {
	f := newTestRunner()
	logs := &stdoutRunner{fakeRunner: f, stdout: "fatal: config invalid\n"}
	t.Setenv("CONTAINERGO_KEEP", "1")

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(logs), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: errors.New("never became ready")}))
	if err == nil {
		t.Fatal("want error when wait strategy fails")
	}
	if f.callWith("delete") != nil {
		t.Fatal("wait rollback issued delete with CONTAINERGO_KEEP=1")
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

			_, err := reuseCreate(context.Background(), "redis:7-alpine", cfg)
			if err == nil {
				t.Fatal("want copy error")
			}
			if got := len(r.deleted) > 0; got != tc.wantDelete {
				t.Fatalf("deleted = %v, want deletion = %t", r.deleted, tc.wantDelete)
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
