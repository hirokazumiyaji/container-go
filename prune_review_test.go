package container

import (
	"context"
	"slices"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestPruneDeleteArgsProtectRunningContainers(t *testing.T) {
	for _, eng := range []engine{dockerEngine{}, appleEngine{}} {
		if slices.Contains(pruneDeleteArgs(eng, "candidate", ""), "--force") {
			t.Fatalf("%s ordinary prune forces deletion", eng.name())
		}
		if !slices.Contains(pruneDeleteArgs(eng, "candidate", "group"), "--force") {
			t.Fatalf("%s group prune lost force", eng.name())
		}
	}
}

func TestPruneAppleDoesNotCountAlreadyRemoved(t *testing.T) {
	fixture := pruneFixture("already-gone", "0123456789abcdef", "stopped", "", true)
	runner := &pruneSafetyRunner{list: []pruneFixtureContainer{fixture}, inspects: []pruneFixtureContainer{fixture}, deleteErr: &cli.CLIError{Args: []string{"delete", "already-gone"}, ExitCode: 1, Stderr: `Error: container not found: "already-gone"`}}
	removed, err := pruneWith(context.Background(), runner, appleEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed=%v, want no successful deletions", removed)
	}
}
