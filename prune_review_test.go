package container

import (
	"context"
	"errors"
	"fmt"
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

func TestPruneSkipsCandidateRestartedDuringDelete(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			stopped := pruneFixture("restart-race", "0123456789abcdef", "stopped", "", true)
			current := stopped
			if running {
				current.Status.State = "running"
			}
			failure := &cli.CLIError{Args: []string{"delete", "restart-race"}, ExitCode: 1, Stderr: "container is running"}
			runner := &pruneSafetyRunner{list: []pruneFixtureContainer{stopped}, inspects: []pruneFixtureContainer{stopped, current}, deleteErr: failure}
			removed, err := pruneWith(context.Background(), runner, appleEngine{})
			if running {
				if err != nil || len(removed) != 0 {
					t.Fatalf("restarted prune=(%v,%v), want skip", removed, err)
				}
			} else if err == nil {
				t.Fatal("delete failure on stopped container was discarded")
			}
		})
	}
}

type dockerPruneDeleteRaceRunner struct {
	running      bool
	deleteOutput []byte
	inspections  int
	group        string
}

func (r *dockerPruneDeleteRaceRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		return []byte(cleanupDockerIDOne + "\n"), nil, nil
	case "inspect":
		r.inspections++
		state := "exited"
		if r.running && r.inspections > 1 {
			state = "running"
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":%q},"Config":{"Labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:%q}}}]`, cleanupDockerIDOne, state, managedLabel, reuseLabel, creationLabel, reuseGroupLabel, r.group)), nil, nil
	case "rm":
		if r.running {
			return nil, nil, &cli.CLIError{Binary: "docker", Args: args, ExitCode: 1, Stderr: "cannot remove container: container is running"}
		}
		return r.deleteOutput, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestPruneDockerSkipsRestartedCandidate(t *testing.T) {
	r := &dockerPruneDeleteRaceRunner{running: true}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err != nil || len(removed) != 0 {
		t.Fatalf("prune=(%v,%v), want skip", removed, err)
	}
}

func TestPruneDockerForceDeleteEmptyOutputIsAlreadyAbsent(t *testing.T) {
	for _, output := range []string{"", cleanupDockerIDOne + "\n"} {
		r := &dockerPruneDeleteRaceRunner{group: "race-group", deleteOutput: []byte(output)}
		removed, err := pruneReuseGroupWith(context.Background(), r, dockerEngine{}, r.group)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if output != "" {
			want = 1
		}
		if len(removed) != want {
			t.Fatalf("stdout=%q removed=%v, want count%d", output, removed, want)
		}
	}
}

func TestNestedNonCLIBranchesVetoAbsenceConsistently(t *testing.T) {
	for depth := 0; depth < 4; depth++ {
		for _, veto := range []error{errors.New("TLS certificate verification failed"), errors.New("workload says no such container")} {
			absent := &cli.CLIError{Binary: "container", Args: []string{"inspect", "candidate"}, ExitCode: 1, Stderr: `Error: notFound: "container not found: candidate"`}
			err := errors.Join(absent, veto)
			for range depth {
				err = fmt.Errorf("wrapper: %w", err)
			}
			if isNotFoundFor(appleEngine{}, err) || isNotFoundForOperation(appleEngine{}, err, "inspect", "candidate") {
				t.Fatalf("depth=%d veto=%v classified absent", depth, veto)
			}
		}
	}
}
