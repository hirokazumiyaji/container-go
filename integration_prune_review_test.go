//go:build integration

package container

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/integrationtest"
)

func TestIntegrationApplePruneNonForceDeleteMissing(t *testing.T) {
	integrationtest.Preflight(t, "apple", integrationtest.AppleUnavailable)
	eng := appleEngine{}
	target := fmt.Sprintf("container-go-missing-%d-%d", os.Getpid(), time.Now().UnixNano())
	args := pruneDeleteArgs(eng, target, "")
	if slices.Contains(args, "--force") {
		t.Fatal("ordinary prune force-deletes")
	}
	_, _, err := (&cli.ExecRunner{Binary: eng.binary()}).Run(context.Background(), args...)
	if err == nil || !isNotFoundForOperation(eng, err, cli.CommandOperation(args), target) {
		t.Fatalf("delete missing: %v, want verified container absence", err)
	}
}

type liveDockerPruneRaceRunner struct {
	runner    cli.Runner
	id        string
	operation string
	injected  atomic.Bool
}

func (r *liveDockerPruneRaceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		return []byte(r.id + "\n"), nil, nil
	}
	if args[0] == "rm" && r.injected.CompareAndSwap(false, true) {
		mutate := []string{"start", r.id}
		if r.operation == "remove" {
			mutate = []string{"rm", "--force", "--volumes", r.id}
		}
		if _, _, err := r.runner.Run(ctx, mutate...); err != nil {
			return nil, nil, err
		}
	}
	return r.runner.Run(ctx, args...)
}

func TestIntegrationDockerPruneDeleteRaces(t *testing.T) {
	requireDockerPruneIntegration(t)
	for _, operation := range []string{"restart", "remove"} {
		t.Run(operation, func(t *testing.T) {
			name := fmt.Sprintf("container-go-prune-race-%s-%d-%d", operation, os.Getpid(), time.Now().UnixNano())
			args := []string{"run", "--detach", "--name", name, "--label", managedLabel + "=true", "--label", creationLabel + "=0123456789abcdef"}
			group := ""
			if operation == "remove" {
				group = "review-race"
				args = append(args, "--label", reuseLabel+"=true", "--label", reuseGroupLabel+"="+group)
			}
			args = append(args, integrationAlpine, "sleep", "300")
			id := runDockerPruneCommand(t, args...)
			t.Cleanup(func() { _ = runDockerPruneCleanup(id) })
			runDockerPruneCommand(t, "kill", id)
			waitDockerContainerState(t, id, "exited")
			runner := &liveDockerPruneRaceRunner{runner: &cli.ExecRunner{Binary: "docker"}, id: strings.TrimSpace(id), operation: operation}
			var removed []string
			var err error
			if group == "" {
				removed, err = pruneWith(context.Background(), runner, dockerEngine{})
			} else {
				removed, err = pruneReuseGroupWith(context.Background(), runner, dockerEngine{}, group)
			}
			if err != nil || len(removed) != 0 {
				t.Fatalf("race prune=(%v,%v), want skipped candidate", removed, err)
			}
			if !runner.injected.Load() {
				t.Fatal("test did not reach deletion")
			}
			if operation == "restart" {
				waitDockerContainerState(t, id, "running")
			}
		})
	}
}

func runDockerPruneCleanup(id string) error {
	_, _, err := (&cli.ExecRunner{Binary: "docker"}).Run(context.Background(), "rm", "--force", "--volumes", id)
	return err
}
