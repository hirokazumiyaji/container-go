//go:build windows

package container

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// TestWindowsRunnerEnvKeysFoldCase pins that a runner environment key is
// compared the way cli.MergeEnv merges it. Windows environment variable names
// are case-insensitive, so a lowercase docker_host entry must be replaced by
// the pinned target instead of surviving MergeEnv's last-wins merge.
func TestWindowsRunnerEnvKeysFoldCase(t *testing.T) {
	pinned := []string{"DOCKER_HOST=tcp://pinned:2375", "DOCKER_CONTEXT", "DOCKER_CONFIG"}

	got := pinRunnerEnv([]string{"docker_host=tcp://runner:2375"}, pinned)
	want := []string{"DOCKER_HOST=tcp://pinned:2375", "DOCKER_CONTEXT", "DOCKER_CONFIG"}
	if !slices.Equal(got, want) {
		t.Fatalf("pinRunnerEnv() = %v, want %v", got, want)
	}

	// A lowercase bare key is the runner's own removal marker, so the pinned
	// marker must not be appended a second time.
	got = pinRunnerEnv([]string{"docker_context", "KEEP=1"}, pinned)
	want = []string{"docker_context", "KEEP=1", "DOCKER_HOST=tcp://pinned:2375", "DOCKER_CONFIG"}
	if !slices.Equal(got, want) {
		t.Fatalf("pinRunnerEnv() = %v, want %v", got, want)
	}

	merged := cli.MergeEnv([]string{
		"DOCKER_HOST=tcp://late:2375",
		"DOCKER_CONTEXT=late",
		"DOCKER_CONFIG=/late",
		"KEEP=0",
	}, got)
	want = []string{"KEEP=1", "DOCKER_HOST=tcp://pinned:2375"}
	if !slices.Equal(merged, want) {
		t.Fatalf("merged env = %v, want %v", merged, want)
	}
}

func TestWindowsRunnerEnvEntryFoldsCase(t *testing.T) {
	f := &fakeRunner{env: []string{"docker_host=tcp://runner:2375"}}
	if entry, ok := runnerEnvEntry(f, "DOCKER_HOST"); !ok || entry != "docker_host=tcp://runner:2375" {
		t.Fatalf("runnerEnvEntry() = %q, %v, want the lowercase entry", entry, ok)
	}
	if value, ok := runnerEnvLookup(f, "DOCKER_HOST"); !ok || value != "tcp://runner:2375" {
		t.Fatalf("runnerEnvLookup() = %q, %v, want tcp://runner:2375", value, ok)
	}

	f.env = []string{"docker_context=first", "DOCKER_CONTEXT=last"}
	if value, ok := runnerEnvLookup(f, "DOCKER_CONTEXT"); !ok || value != "last" {
		t.Fatalf("runnerEnvLookup() = %q, %v, want the last occurrence", value, ok)
	}
}

// TestWindowsPruneReadsLowercaseBareContextMarker pins the prune path that
// decides whether the runner removed DOCKER_CONTEXT. The comparison must fold
// case like runnerEnvEntry does.
func TestWindowsPruneReadsLowercaseBareContextMarker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "ambient")
	f := newDockerTestRunner()
	f.env = []string{"docker_context"}

	if _, err := (dockerEngine{}).preparePruneTarget(context.Background(), f, time.Second); err != nil {
		t.Fatalf("preparePruneTarget: %v", err)
	}
	if call := f.callWith("context"); !slices.Equal(call, []string{"context", "inspect"}) {
		t.Fatalf("context inspect args = %v, want the active context (the marker removes DOCKER_CONTEXT)", call)
	}
}
