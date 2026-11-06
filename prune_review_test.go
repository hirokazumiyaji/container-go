package container

import (
	"context"
	"errors"
	"fmt"
	"io"
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

type refusalRaceRunner struct {
	eng            engine
	after          string
	cancel         context.CancelFunc
	inspections    int
	failure        error
	inspectFailure error
}

func (r *refusalRaceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	id := "refusal-race"
	if r.eng.name() == "docker" {
		id = cleanupDockerIDOne
	}
	fixture := pruneFixture(id, "0123456789abcdef", "stopped", "", true)
	switch args[0] {
	case "ps":
		return []byte(id + "\n"), nil, nil
	case "ls":
		return mustPruneJSON([]pruneFixtureContainer{fixture}), nil, nil
	case "rm", "delete":
		return nil, nil, r.failure
	case "inspect":
		r.inspections++
		if r.inspections > 1 {
			if r.inspectFailure != nil {
				return nil, nil, r.inspectFailure
			}
			if r.after == "cancel" {
				r.cancel()
				return nil, nil, ctx.Err()
			}
			if r.after == "missing" {
				message := `Error: notFound: "container not found: ` + id + `"`
				if r.eng.name() == "docker" {
					message = "Error: No such container: " + id
				}
				return nil, nil, &cli.CLIError{Binary: r.eng.binary(), Args: args, ExitCode: 1, Stderr: message}
			}
			fixture.Status.State = r.after
		}
		if r.eng.name() == "apple" {
			return mustPruneJSON([]pruneFixtureContainer{fixture}), nil, nil
		}
		state := fixture.Status.State
		if state == "stopped" {
			state = "exited"
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/refusal-race","State":{"Status":%q},"Config":{"Labels":{%q:"true",%q:"0123456789abcdef",%q:%q}}}]`, id, state, managedLabel, creationLabel, sessionLabel, pruneFixtureSession)), nil, nil
	}
	return nil, nil, nil
}

func TestPruneDeleteRefusalRaces(t *testing.T) {
	for _, eng := range []engine{dockerEngine{}, appleEngine{}} {
		for _, after := range []string{"paused", "restarting", "missing", "cancel", "stopped", "unknown", "created", "stopping", "inspect-error"} {
			t.Run(eng.name()+"/"+after, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("backend refused deletion")
				inspectFailure := errors.New("TLS reinspection failed")
				r := &refusalRaceRunner{eng: eng, after: after, cancel: cancel, failure: failure}
				if after == "inspect-error" {
					r.inspectFailure = inspectFailure
				}
				removed, err := pruneWith(ctx, r, eng)
				switch after {
				case "cancel":
					if !errors.Is(err, context.Canceled) || !errors.Is(err, failure) {
						t.Fatalf("got %v, want cancellation and original deletion failure", err)
					}
				case "stopped", "unknown", "created", "stopping", "inspect-error":
					if !errors.Is(err, failure) {
						t.Fatalf("lost deletion failure: %v", err)
					}
					if after == "inspect-error" && !errors.Is(err, inspectFailure) {
						t.Fatalf("lost reinspection failure: %v", err)
					}
				default:
					if eng.name() == "apple" && (after == "paused" || after == "restarting") {
						if !errors.Is(err, failure) {
							t.Fatalf("unknown Apple status lost deletion failure: %v", err)
						}
						return
					}
					if err != nil || len(removed) != 0 {
						t.Fatalf("prune=(%v,%v), want skipped race", removed, err)
					}
				}
			})
		}
	}
}

func TestPruneDockerNonForceAcceptsRunnerWithoutStdout(t *testing.T) {
	r := &dockerPruneDeleteRaceRunner{}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err != nil || len(removed) != 1 {
		t.Fatalf("prune=(%v,%v), want one deletion", removed, err)
	}
}

func TestNonCLIPureWrapperTextVetoesAbsence(t *testing.T) {
	absent := &cli.CLIError{Binary: "docker", Args: []string{"inspect", "missing"}, ExitCode: 1, Stderr: "Error: No such container: missing"}
	transport := fmt.Errorf("TLS certificate verification failed: %w", fmt.Errorf("dial: %w", errors.Join(io.EOF, io.ErrUnexpectedEOF)))
	if isNotFoundFor(dockerEngine{}, errors.Join(absent, transport)) {
		t.Fatal("lost non-CLI wrapper TLS veto")
	}
	confirmed := fmt.Errorf("%w: %w", ErrContainerNotFound, absent)
	if !isNotFoundFor(dockerEngine{}, errors.Join(confirmed, errors.New("no such object: x"))) {
		t.Fatal("lost confirmed absence sentinel")
	}
}

func TestPruneDeleteRaceRequiresProvenRevival(t *testing.T) {
	for _, state := range []State{StateUnknown, StateCreated, StateStopping, StateStopped, StateRunning, StateRestarting, StatePaused} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			skip, absent, err := pruneDeleteRace(context.Background(), dockerEngine{}, func(context.Context) (*engineInfo, error) { return &engineInfo{state: state}, nil })
			wantSkip := state == StateRunning || state == StateRestarting || state == StatePaused
			if skip != wantSkip || absent || err != nil {
				t.Fatalf("race = %v/%v/%v, want skip %v", skip, absent, err, wantSkip)
			}
		})
	}
	failure := errors.New("reinspection failed")
	skip, absent, err := pruneDeleteRace(context.Background(), dockerEngine{}, func(context.Context) (*engineInfo, error) { return nil, failure })
	if skip || absent || !errors.Is(err, failure) {
		t.Fatalf("lost reinspection failure: %v/%v/%v", skip, absent, err)
	}
}

func TestRedactedJoinedVetoPreventsAbsence(t *testing.T) {
	for _, eng := range []engine{dockerEngine{}, appleEngine{}} {
		message := "Error: No such container: candidate"
		if eng.name() == "apple" {
			message = `Error: notFound: "container not found: candidate"`
		}
		absent := &cli.CLIError{Binary: eng.binary(), Args: []string{"inspect", "candidate"}, ExitCode: 1, Stderr: message}
		err := cli.WithRedactor(errors.Join(absent, errors.New("TLS certificate verification failed private-value")), cli.NewRedactor("private-value"))
		if isNotFoundFor(eng, err) || isNotFoundForOperation(eng, err, "inspect", "candidate") {
			t.Fatalf("%s discarded redacted TLS veto", eng.name())
		}
	}
}
