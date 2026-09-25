package container

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type reviewOperationRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *reviewOperationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	if args[0] == "inspect" {
		return []byte(`[{"Id":"` + reviewReaperDockerID + `","Name":"/logical-name","State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{"` + managedLabel + `":"true","` + creationLabel + `":"aaaaaaaaaaaaaaaa"}},"NetworkSettings":{}}]`), nil, nil
	}
	return nil, nil, nil
}

func (r *reviewOperationRunner) snapshotCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

func reviewContainsTarget(calls [][]string, target string) bool {
	for _, args := range calls {
		for _, arg := range args {
			if arg == target || strings.HasPrefix(arg, target+":") {
				return true
			}
		}
	}
	return false
}

type reviewStreamRunner struct {
	*reviewOperationRunner
	args []string
}

func (r *reviewStreamRunner) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	r.args = append([]string(nil), args...)
	return io.NopCloser(strings.NewReader("streamed")), nil
}

func TestReviewDockerOperationsUseImmutableID(t *testing.T) {
	runner := &reviewOperationRunner{}
	ctr := &Container{id: "logical-name", uid: reviewReaperDockerID, runner: runner, eng: dockerEngine{}}
	input := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(context.Background(), input, "/tmp/input"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatal(err)
	}
	logs, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = logs.Close()
	calls := runner.snapshotCalls()
	if !reviewContainsTarget(calls, reviewReaperDockerID) {
		t.Fatalf("operations did not use immutable ID: %v", calls)
	}
	if reviewContainsTarget(calls, "logical-name") {
		t.Fatalf("operation addressed logical name: %v", calls)
	}

	streamRunner := &reviewStreamRunner{reviewOperationRunner: &reviewOperationRunner{}}
	streamCtr := &Container{id: "logical-name", uid: reviewReaperDockerID, runner: streamRunner, eng: dockerEngine{}}
	stream, err := streamCtr.FollowLogs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if len(streamRunner.args) == 0 || !reviewContainsTarget([][]string{streamRunner.args}, reviewReaperDockerID) || reviewContainsTarget([][]string{streamRunner.args}, "logical-name") {
		t.Fatalf("FollowLogs args = %v, want immutable ID", streamRunner.args)
	}
}

func TestReviewDockerOperationRejectsInvalidImmutableID(t *testing.T) {
	runner := &reviewOperationRunner{}
	ctr := &Container{id: "logical-name", uid: "short", runner: runner, eng: dockerEngine{}}
	if err := ctr.Stop(context.Background(), nil); err == nil {
		t.Fatal("Stop accepted a non-immutable Docker ID")
	}
	empty := &Container{id: "logical-name", runner: runner, eng: dockerEngine{}}
	if err := empty.Stop(context.Background(), nil); err == nil {
		t.Fatal("Stop accepted a Docker handle without an immutable ID")
	}
}

func TestReviewAppleOperationRequiresOwnershipAndKnownState(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	runner := &reviewAppleInspectRunner{state: StateUnknown}
	ctr := &Container{id: "apple-review", creation: "aaaaaaaaaaaaaaaa", runner: runner, eng: appleEngine{}}
	if err := ctr.Stop(context.Background(), nil); err == nil {
		t.Fatal("Stop accepted unknown/unowned Apple state")
	}
	if runner.calls != 1 || len(runner.operationIDs) != 0 {
		t.Fatalf("inspect calls = %d, operation calls = %v; want one inspect and no operation", runner.calls, runner.operationIDs)
	}
}

func TestReviewAppleOperationRejectsUnrecognizedState(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	runner := &reviewAppleInspectRunner{state: State("future-state")}
	ctr := &Container{id: "apple-review", creation: "aaaaaaaaaaaaaaaa", runner: runner, eng: appleEngine{}}
	if err := ctr.Stop(context.Background(), nil); err == nil {
		t.Fatal("Stop accepted an unrecognized Apple state")
	}
	if runner.calls != 1 || len(runner.operationIDs) != 0 {
		t.Fatalf("inspect calls = %d, operation calls = %v; want one inspect and no operation", runner.calls, runner.operationIDs)
	}
}

type reviewAppleInspectRunner struct {
	state        State
	calls        int
	operationIDs []string
}

func (r *reviewAppleInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.calls++
		return []byte(`[{"id":"apple-review","configuration":{"id":"apple-review","image":{"reference":"redis"},"labels":{"` + managedLabel + `":"true","` + sessionLabel + `":"` + sessionID() + `","` + creationLabel + `":"aaaaaaaaaaaaaaaa"}},"status":{"state":"` + string(r.state) + `","networks":[]}}]`), nil, nil
	}
	r.operationIDs = append(r.operationIDs, args[0])
	return nil, nil, nil
}

var _ cli.Streamer = (*reviewStreamRunner)(nil)
