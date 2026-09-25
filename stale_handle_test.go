package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const staleHandleUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type staleHandleRunner struct {
	calls               [][]string
	streamCalls         [][]string
	replacementRunning  bool
	replacementFileData string
}

func (r *staleHandleRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, args)
	if args[0] == "version" {
		return []byte("29.8.0"), nil, nil
	}
	if hasImmutableTarget(args, staleHandleUID) {
		return nil, nil, notFoundError(args)
	}

	switch args[0] {
	case "inspect":
		status := "running"
		if !r.replacementRunning {
			status = "exited"
		}
		return []byte(`[{"Id":"` + staleHandleUID + `","Name":"/replacement","State":{"Status":"` + status + `"},"Config":{"Image":"alpine"},"NetworkSettings":{"IPAddress":"172.17.0.2"}}]`), nil, nil
	case "exec":
		return []byte("replacement exec output"), nil, nil
	case "cp":
		// A name-targeted copy would operate on the replacement. This
		// makes the pre-fix behavior observable while the UID path fails.
		if len(args) == 3 && strings.Contains(args[1], ":") {
			if err := os.WriteFile(args[2], []byte(r.replacementFileData), 0o600); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil
	case "logs":
		return []byte("replacement logs"), nil, nil
	case "stop":
		r.replacementRunning = false
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *staleHandleRunner) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	r.streamCalls = append(r.streamCalls, args)
	if hasImmutableTarget(args, staleHandleUID) {
		return nil, notFoundError(args)
	}
	return io.NopCloser(strings.NewReader("replacement logs")), nil
}

func notFoundError(args []string) error {
	return &cli.CLIError{
		Args:     args,
		ExitCode: 1,
		Stderr:   "Error response from daemon: No such container: " + staleHandleUID,
	}
}

func hasImmutableTarget(args []string, uid string) bool {
	for _, arg := range args {
		if arg == uid || strings.HasPrefix(arg, uid+":") {
			return true
		}
	}
	return false
}

func TestDockerHandleOperationsUseImmutableID(t *testing.T) {
	r := &staleHandleRunner{
		replacementRunning:  true,
		replacementFileData: "replacement data",
	}
	ctr := &Container{
		id:     "replacement",
		uid:    staleHandleUID,
		runner: r,
		eng:    dockerEngine{},
	}
	ctx := context.Background()

	if ctr.ID() != "replacement" {
		t.Fatalf("ID() = %q, want logical name", ctr.ID())
	}
	if _, err := ctr.State(ctx); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("State error = %v, want ErrContainerNotFound", err)
	}
	if _, _, err := ctr.Exec(ctx, []string{"true"}); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("Exec error = %v, want ErrContainerNotFound", err)
	}
	src := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(src, []byte("stale input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(ctx, src, "/tmp/stale-copy.txt"); !isNotFound(err) {
		t.Errorf("CopyToContainer error = %v, want backend not-found", err)
	}
	if _, err := ctr.CopyFileFromContainer(ctx, "/tmp/replacement.txt"); !isNotFound(err) {
		t.Errorf("CopyFileFromContainer error = %v, want backend not-found", err)
	}
	if _, err := ctr.Logs(ctx); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("Logs error = %v, want ErrContainerNotFound", err)
	}
	if _, err := ctr.LogsWithOptions(ctx, LogsOptions{Tail: 10}); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("LogsWithOptions error = %v, want ErrContainerNotFound", err)
	}
	if _, err := ctr.FollowLogs(ctx); !isNotFound(err) {
		t.Errorf("FollowLogs error = %v, want backend not-found", err)
	}
	if err := ctr.Stop(ctx, nil); !isNotFound(err) {
		t.Errorf("Stop error = %v, want backend not-found", err)
	}
	if _, err := ctr.ContainerIP(ctx); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("ContainerIP error = %v, want ErrContainerNotFound", err)
	}
	ctr.exposed = []portSpec{{port: 80, proto: "tcp"}}
	if _, err := ctr.Endpoint(ctx, "80/tcp"); !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("Endpoint error = %v, want ErrContainerNotFound", err)
	}
	if tail := ctr.logTail(ctx); tail != "" {
		t.Errorf("logTail = %q, want empty for missing immutable target", tail)
	}
	if err := ctr.Terminate(ctx); err != nil {
		t.Errorf("Terminate on missing immutable target = %v, want idempotent success", err)
	}
	if !r.replacementRunning {
		t.Error("stale handle changed the replacement state")
	}
	assertUIDTargets(t, r.calls, staleHandleUID)
	if len(r.streamCalls) != 1 || !hasImmutableTarget(r.streamCalls[0], staleHandleUID) {
		t.Errorf("FollowLogs args = %v, want immutable ID", r.streamCalls)
	}
}

func assertUIDTargets(t *testing.T, calls [][]string, uid string) {
	t.Helper()
	for _, call := range calls {
		if call[0] == "version" {
			continue
		}
		if !hasImmutableTarget(call, uid) {
			t.Errorf("operation args = %v, want target %s", call, uid)
		}
	}
}

func TestAppleHandleKeepsLogicalNameTarget(t *testing.T) {
	runner := newTestRunner()
	ctr := &Container{
		id:     "apple-handle",
		eng:    appleEngine{},
		runner: runner,
	}
	if got := ctr.operationTarget(); got != "apple-handle" {
		t.Fatalf("operationTarget() = %q, want logical name", got)
	}
	if got := ctr.ID(); got != "apple-handle" {
		t.Fatalf("ID() = %q, want logical name", got)
	}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if call := runner.callWith("stop"); !slices.Equal(call, []string{"stop", "apple-handle"}) {
		t.Fatalf("stop args = %v, want logical name", call)
	}
}
