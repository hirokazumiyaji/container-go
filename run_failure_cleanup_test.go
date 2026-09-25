package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// failRunRunner fails the run call but serves an owned inspect payload
// so cleanupFailedCreate can verify ownership.
type failRunRunner struct {
	*fakeRunner
	runErr      error
	inspectJSON string
	inspectErr  error
	deleteErr   error
	probeErr    error
	creation    string
	deleted     []string
}

func (r *failRunRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "system":
		if r.probeErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.probeErr
		}
		return r.fakeRunner.Run(ctx, args...)
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i := 0; i+1 < len(args); i++ {
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				r.creation = value
				break
			}
		}
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		creation := r.creation
		r.mu.Unlock()
		if r.inspectErr != nil {
			return nil, nil, r.inspectErr
		}
		return []byte(strings.ReplaceAll(r.inspectJSON, "__CONTAINER_CREATION__", creation)), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		if r.deleteErr != nil {
			return nil, nil, r.deleteErr
		}
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func ownedInspectJSON(name string) string {
	return ownedInspectJSONWithState(name, "created", false)
}

func ownedReuseInspectJSON(name string) string {
	return ownedInspectJSONWithState(name, "created", true)
}

func ownedInspectJSONWithState(name, state string, reuse bool) string {
	reuseLabel := ""
	if reuse {
		reuseLabel = `,
        "com.github.hirokazumiyaji.container-go.reuse": "true"`
	}
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.session": %q%s,
        "com.github.hirokazumiyaji.container-go.creation": "__CONTAINER_CREATION__"
      }
    },
    "status": {"state": %q, "networks": []}
  }
]`, name, name, sessionID(), reuseLabel, state)
}

func foreignInspectJSON(name string) string {
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {"com.github.hirokazumiyaji.container-go": "true"}
    },
    "status": {"state": "created", "networks": []}
  }
]`, name, name)
}

func TestRunFailureCleansUpOwnedContainer(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error for failed run")
	}
	if len(r.deleted) != 1 || r.deleted[0] != "myctr" {
		t.Fatalf("deleted = %v, want [myctr]", r.deleted)
	}
}

func TestRunFailurePreservesCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "daemon unavailable"}
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedInspectJSON("myctr"),
		deleteErr:   cleanupErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
	if !strings.Contains(err.Error(), "cleanup") || !strings.Contains(err.Error(), "myctr") {
		t.Fatalf("error = %v, want cleanup context and container name", err)
	}
}

func TestRunFailurePreservesBothCLIErrorsWhenBackendIsDown(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "daemon unavailable"}
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedInspectJSON("myctr"),
		deleteErr:   cleanupErr,
		probeErr:    &cli.CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "backend down"},
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
}

func TestRunFailurePreservesCleanupInspectCLIError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "cleanup inspect failed"}
	r := &failRunRunner{
		fakeRunner: base,
		runErr:     runErr,
		inspectErr: cleanupErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup inspect CLIError", err)
	}
}

func TestRunFailureCleanupNotFoundIsSuccess(t *testing.T) {
	cases := []struct {
		name       string
		inspectErr error
		deleteErr  error
	}{
		{
			name:       "inspect",
			inspectErr: &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: `not found: "myctr"`},
		},
		{
			name:      "delete",
			deleteErr: &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: `not found: "myctr"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestRunner()
			base.imagePresent = true
			r := &failRunRunner{
				fakeRunner:  base,
				runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"},
				inspectJSON: ownedInspectJSON("myctr"),
				inspectErr:  tc.inspectErr,
				deleteErr:   tc.deleteErr,
			}

			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
			var cleanupErr *CleanupError
			if errors.As(err, &cleanupErr) {
				t.Fatalf("error = %v, want not-found cleanup to be successful", err)
			}
			if got := cliErrorWithStderr(err, "start failed"); got == nil {
				t.Fatalf("error = %v, want original run CLIError", err)
			}
		})
	}
}

func TestRunFailurePreservesNameConflict(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner: base,
		runErr: &cli.CLIError{Args: []string{"run"}, ExitCode: 1,
			Stderr: `Error: already exists: container "myctr"`},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want conflict error")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no cleanup on name conflict", r.deleted)
	}
}

func TestRunFailureConfigErrorStillCleansOwnedDockerContainer(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{
		Binary: "docker", Args: []string{"run", "--name", "myctr"}, ExitCode: 125,
		Stderr: "image config contains a conflict",
	}
	inspectJSON := fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{"%s":"true","%s":%q,"%s":"__CONTAINER_CREATION__"}}}]`, uid, managedLabel, sessionLabel, sessionID(), creationLabel)
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: inspectJSON,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}))
	if got := cliErrorWithStderr(err, runErr.Stderr); got != runErr {
		t.Fatalf("error = %v, want original config CLIError", err)
	}
	if len(r.deleted) != 1 || r.deleted[0] != uid {
		t.Fatalf("deleted = %v, want immutable ID %s", r.deleted, uid)
	}
}

func TestRunFailurePreservesForeignContainer(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "port bind failed"},
		inspectJSON: foreignInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no cleanup for foreign container", r.deleted)
	}
}

func TestRunFailureCleansUpAfterCancel(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = appleEngine{}
	cfg.name = "myctr"
	cfg.creation = "0123456789abcdef"
	r.creation = cfg.creation
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	if err := cleanupFailedCreate(ctx, cfg, runErr, runErr); err != nil {
		t.Fatalf("cleanupFailedCreate: %v", err)
	}
	if len(r.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup even after cancel", r.deleted)
	}
}

func TestReuseCreateFailureCleansUpOwned(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	// reuseCreate inspects first: report not-found once, then owned after failed run.
	calls := 0
	inner := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedReuseInspectJSON("myctr"),
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: &calls}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("error = %v, want run failure", err)
	}
	if len(inner.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup", inner.deleted)
	}
}

type rollbackErrorRunner struct {
	*fakeRunner
	copyErr   error
	execErr   error
	logsErr   error
	deleteErr error
}

func (r *rollbackErrorRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "exec":
		if r.execErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.execErr
		}
	case "logs":
		if r.logsErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.logsErr
		}
	case "cp":
		if r.copyErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.copyErr
		}
	case "delete", "rm":
		if r.deleteErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.deleteErr
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *rollbackErrorRunner) Stream(_ context.Context, _ ...string) (io.ReadCloser, error) {
	if r.logsErr != nil {
		return nil, r.logsErr
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func TestRunCopyFailurePreservesCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	copyErr := &cli.CLIError{Args: []string{"cp"}, ExitCode: 1, Stderr: "copy failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "delete failed"}
	r := &rollbackErrorRunner{fakeRunner: base, copyErr: copyErr, deleteErr: cleanupErr}
	hostPath := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(hostPath, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: hostPath, ContainerPath: "/tmp/input.txt"}))
	if got := cliErrorWithStderr(err, copyErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original copy CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
}

func TestRunWaitFailurePreservesCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	waitErr := &cli.CLIError{Args: []string{"wait"}, ExitCode: 1, Stderr: "wait failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "delete failed"}
	r := &rollbackErrorRunner{fakeRunner: base, deleteErr: cleanupErr}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: waitErr}))
	if got := cliErrorWithStderr(err, waitErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original wait CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
}

func TestBuiltInWaitFailurePreservesPrimaryAndCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	waitErr := &cli.CLIError{Args: []string{"logs", "myctr"}, ExitCode: 1, Stderr: "logs failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "delete failed"}
	r := &rollbackErrorRunner{fakeRunner: base, logsErr: waitErr, deleteErr: cleanupErr}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithWaitStrategy(wait.ForLog("ready").
			WithStartupTimeout(25*time.Millisecond).
			WithPollInterval(time.Millisecond)))
	var cleanupErrType *CleanupError
	if !errors.As(err, &cleanupErrType) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if got := cliErrorWithStderr(cleanupErrType.Err, waitErr.Stderr); got != waitErr {
		t.Fatalf("primary error = %v, want %v", got, waitErr)
	}
	if got := cliErrorWithStderr(cleanupErrType.CleanupErr, cleanupErr.Stderr); got != cleanupErr {
		t.Fatalf("cleanup error = %v, want %v", got, cleanupErr)
	}
}

func TestReuseFailedCreateDoesNotDeleteRunningGeneration(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	inner := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"},
		inspectJSON: ownedInspectJSONWithState("myctr", "running", true),
	}
	calls := 0
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: &calls}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want failed create error")
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if !strings.Contains(cleanupErr.CleanupErr.Error(), "running reuse generation") {
		t.Fatalf("cleanup error = %v, want running-generation refusal", cleanupErr.CleanupErr)
	}
	if len(inner.deleted) != 0 {
		t.Fatalf("deleted = %v, want no automatic delete of running reuse generation", inner.deleted)
	}
}

func TestReuseRetryClassificationIgnoresCleanupConflict(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	primaryErr := errors.New("primary create failed")
	cleanupErr := &cli.CLIError{
		Args:     []string{"delete", "myctr"},
		ExitCode: 1,
		Stderr:   `Conflict. The container name "myctr" is already in use by container peer`,
	}
	r := &reusePrimaryConflictRunner{fakeRunner: base, primaryErr: primaryErr, cleanupErr: cleanupErr}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if ctr != nil {
		t.Fatalf("container = %v, want nil after failed create", ctr)
	}
	var combined *CleanupError
	if !errors.As(err, &combined) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if !errors.Is(combined.Err, primaryErr) {
		t.Fatalf("primary error = %v, want %v", combined.Err, primaryErr)
	}
	if cliErrorWithStderr(combined.CleanupErr, cleanupErr.Stderr) != cleanupErr {
		t.Fatalf("cleanup error = %v, want %v", combined.CleanupErr, cleanupErr)
	}
	if r.inspectCalls != 2 {
		t.Fatalf("inspect calls = %d, want failed create inspect plus cleanup inspect", r.inspectCalls)
	}
}

type reusePrimaryConflictRunner struct {
	*fakeRunner
	primaryErr   error
	cleanupErr   error
	creation     string
	inspectCalls int
}

func (r *reusePrimaryConflictRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i := 0; i+1 < len(args); i++ {
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				r.creation = value
				break
			}
		}
		r.mu.Unlock()
		return nil, nil, r.primaryErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspectCalls++
		call := r.inspectCalls
		creation := r.creation
		r.mu.Unlock()
		if call == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `inspect failed: not found: "myctr"`}
		}
		state := "created"
		if call > 2 {
			state = "running"
		}
		return []byte(strings.ReplaceAll(ownedInspectJSONWithState("myctr", state, true), "__CONTAINER_CREATION__", creation)), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.cleanupErr
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

type reuseFailWrapper struct {
	*failRunRunner
	calls *int
}

func (w *reuseFailWrapper) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		*w.calls++
		if *w.calls == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
		}
	}
	return w.failRunRunner.Run(ctx, args...)
}
