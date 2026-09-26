package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// A cancelled child is signalled, so its -1 exit code is not an app result.
// IsCommandExit must veto it, otherwise Exec reports "exit -1, no error" and
// a caller cancellation silently becomes a success.
func TestReviewIsCommandExitVetoesContextError(t *testing.T) {
	cliErr := &cli.CLIError{Binary: "docker", Args: []string{"exec", "x"}, ExitCode: -1}

	if !cli.IsCommandExit(cliErr) {
		t.Fatal("plain CLIError should be a command exit")
	}
	if cli.IsCommandExit(nil) {
		t.Error("nil must not be a command exit")
	}
	if cli.IsCommandExit(errors.Join(cliErr, context.DeadlineExceeded)) {
		t.Error("CLIError joined with DeadlineExceeded must not be a command exit")
	}
	if cli.IsCommandExit(errors.Join(cliErr, context.Canceled)) {
		t.Error("CLIError joined with Canceled must not be a command exit")
	}
}

// A matched-but-malformed inspect record must not be reported as
// ErrContainerNotFound: callers treat that as already gone and would leak a
// live container.
func TestReviewDockerInspectMalformedRecordIsNotNotFound(t *testing.T) {
	id := strings.Repeat("a", 64)
	eng := dockerEngine{}

	// Matches by ID but carries no State.
	malformed := `[{"Id":"` + id + `","Name":"/x","Config":{"Labels":{}}}]`
	_, err := eng.parseInspect([]byte(malformed), id)
	if err == nil {
		t.Fatal("malformed record unexpectedly parsed")
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("malformed record reported as not-found (would leak a live container): %v", err)
	}
	if !strings.Contains(err.Error(), "no container-shaped state") {
		t.Errorf("unexpected error: %v", err)
	}

	wellFormed := `[{"Id":"` + id + `","Name":"/x","State":{"Status":"running"},"Config":{"Labels":{}}}]`
	if _, err := eng.parseInspect([]byte(wellFormed), id); err != nil {
		t.Fatalf("well-formed record failed to parse: %v", err)
	}

	absent := `[{"Id":"` + strings.Repeat("b", 64) + `","Name":"/y","State":{"Status":"running"},"Config":{"Labels":{}}}]`
	if _, err := eng.parseInspect([]byte(absent), id); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("absent target should be not-found, got %v", err)
	}
}

// PruneReuseGroup force-removes every container tagged with the group, so a
// group member without the managed label must still be deleted.
func TestReviewPruneReuseGroupIgnoresManagedLabel(t *testing.T) {
	creation := strings.Repeat("c", 16)
	candidate := pruneCandidate{
		id:         "ctr-a",
		creation:   creation,
		state:      StateRunning,
		managed:    false,
		reuseGroup: "grp",
	}
	fresh := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{creationLabel: creation, reuseGroupLabel: "grp"},
	}
	if !pruneCandidateStillCurrent(candidate, fresh, "grp") {
		t.Error("reuse-group member without managed label was skipped")
	}

	// Ordinary Prune still requires the managed label and a stopped state.
	if pruneCandidateStillCurrent(candidate, fresh, "") {
		t.Error("ordinary prune accepted an unmanaged candidate")
	}
	managedRunning := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", creationLabel: creation, reuseGroupLabel: "grp"},
	}
	if pruneCandidateStillCurrent(candidate, managedRunning, "") {
		t.Error("ordinary prune accepted a running container")
	}
}

// docker cp accepts created and stopped containers, so copy must not require
// a running state on that backend.
func TestReviewCopyRequiresRunningIsBackendSpecific(t *testing.T) {
	if !copyRequiresRunning(appleEngine{}) {
		t.Error("apple `container cp` requires a running container")
	}
	if copyRequiresRunning(dockerEngine{}) {
		t.Error("`docker cp` works on created and stopped containers")
	}
}

// A stream must not hold the non-reentrant name lock for its lifetime,
// otherwise two concurrent log readers serialize behind the first.
func TestReviewVerifiedReadTargetReleasesNameLock(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())

	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := ctr.verifiedReadTarget(ctx)
		cancel()
		if err != nil {
			t.Fatalf("verifiedReadTarget call %d: %v", i, err)
		}
	}
}

// The exact not-found matchers must read both streams. A backend that writes
// the diagnostic to stdout would otherwise leave a missing container
// unclassified, and callers treat "not found" as already gone.
func TestReviewNotFoundMatcherReadsStdout(t *testing.T) {
	stdoutOnly := cli.NewStreamCLIError("docker", []string{"inspect", "myctr"}, 1, "", "Error response from daemon: No such object: myctr")
	if !exactContainerNotFoundFor(stdoutOnly, "docker") {
		t.Error("stdout-only not-found was not matched")
	}
	if !isNotFoundFor(dockerEngine{}, stdoutOnly) {
		t.Error("stdout-only not-found did not classify as not-found")
	}
	// The stderr-only form still matches.
	stderrOnly := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"inspect", "myctr"},
		Stderr: "Error response from daemon: No such object: myctr",
	}
	if !exactContainerNotFoundFor(stderrOnly, "docker") {
		t.Error("stderr not-found was not matched")
	}
}

// A reuse-group prune must delete an unmanaged group member; ordinary Prune
// must not.
func TestReviewPruneReuseGroupDeletesUnmanagedMember(t *testing.T) {
	creation := strings.Repeat("c", 16)
	fresh := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{creationLabel: creation, reuseGroupLabel: "grp"},
	}
	if err := verifyDestructiveInfo(appleEngine{}, fresh, false, false); err != nil {
		t.Errorf("reuse-group prune rejected an unmanaged member: %v", err)
	}
	if err := verifyDestructiveInfo(appleEngine{}, fresh, false, true); err == nil {
		t.Error("ordinary prune accepted an unmanaged container")
	}
}

// A stopped container carrying the requested name but belonging to a
// different reuse group must never be terminated by this call.
func TestReviewDeleteStoppedReuseRejectsForeignGroup(t *testing.T) {
	creation := strings.Repeat("c", 16)
	info := &engineInfo{
		state: StateStopped,
		labels: map[string]string{
			reuseLabel:      "true",
			managedLabel:    "true",
			creationLabel:   creation,
			reuseGroupLabel: "other-group",
		},
	}
	cfg := &config{name: "myctr", reuseGroup: "mine"}
	if err := checkReuseGroup(info, cfg); err == nil {
		t.Error("a foreign reuse group was accepted")
	}
	// No group requested means no group constraint.
	if err := checkReuseGroup(info, &config{name: "myctr"}); err != nil {
		t.Errorf("ungrouped call rejected by a group check: %v", err)
	}
	// The matching group passes.
	info.labels[reuseGroupLabel] = "mine"
	if err := checkReuseGroup(info, cfg); err != nil {
		t.Errorf("matching reuse group rejected: %v", err)
	}
}

// With a spawner, Exec must release the name lock once the child exists:
// Exec has no default deadline, so holding the lock for the command would
// block every other name-addressed operation on that container until it
// returns. A real cli.ExecRunner is used because cli.StartedCommand cannot be
// constructed from outside the cli package.
func TestReviewExecDoesNotHoldNameLockDuringCommand(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	// A stub that answers `inspect` for the verification step, and signals
	// then blocks for the exec itself.
	dir := t.TempDir()
	startedFile := dir + "/started"
	releaseFile := dir + "/release"
	inspectJSON := dir + "/inspect.json"
	payload, err := json.Marshal([]map[string]any{{
		"id": ctr.ID(),
		"configuration": map[string]any{
			"id":             ctr.ID(),
			"image":          map[string]any{"reference": "docker.io/library/redis:7-alpine"},
			"publishedPorts": []any{},
			"labels": map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: ctr.creation,
			},
		},
		"status": map[string]any{
			"state":    "running",
			"networks": []any{map[string]string{"ipv4Address": "192.168.64.3/24", "network": "default"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inspectJSON, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	script := dir + "/stub"
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then cat " + inspectJSON + "; exit 0; fi\n" +
		"touch " + startedFile + "\n" +
		"while [ ! -f " + releaseFile + " ]; do sleep 0.02; done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	ctr.runner = &cli.ExecRunner{Binary: script}

	done := make(chan error, 1)
	go func() {
		_, _, err := ctr.Exec(context.Background(), []string{"ignored"})
		done <- err
	}()

	// Wait for the child to be running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = os.WriteFile(releaseFile, nil, 0o600)
			t.Fatalf("the exec child never started; Exec returned: %v", <-done)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The name must be free while the command is still running.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, lockErr := ctr.verifiedReadTarget(ctx)
	cancel()
	_ = os.WriteFile(releaseFile, nil, 0o600)
	if execErr := <-done; execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	if lockErr != nil {
		t.Fatalf("name lock still held while the command ran: %v", lockErr)
	}
}

// blockingExecRunner is a plain Runner with no Spawner. Exec must then hold
// the name lock across the whole command, because there is no way to release
// it between resolving the name and the child process.
type blockingExecRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
}

func (b *blockingExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		select {
		case <-b.started:
		default:
			close(b.started)
		}
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return b.fakeRunner.Run(ctx, args...)
}

// Without a spawner there is no window between the name resolution and the
// child process, so the lock must span the command. Releasing it early would
// let a peer delete and recreate the name, and the command would run in a
// different container.
func TestReviewExecHoldsNameLockWithoutSpawner(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	started := make(chan struct{})
	release := make(chan struct{})
	ctr.runner = &blockingExecRunner{fakeRunner: f, started: started, release: release}

	done := make(chan error, 1)
	go func() {
		_, _, err := ctr.Exec(context.Background(), []string{"true"})
		done <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := ctr.verifiedReadTarget(ctx)
	cancel()
	if err == nil {
		close(release)
		<-done
		t.Fatal("name lock was released mid-command; a peer could recreate the name")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Exec: %v", err)
	}
}
