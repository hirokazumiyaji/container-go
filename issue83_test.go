package container

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestCheckReuseOwnedRequiresAllOwnershipLabels(t *testing.T) {
	const validCreation = "0123456789abcdef"
	cfg := &config{name: "myctr"}
	cases := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{
			name:    "reuse only",
			labels:  map[string]string{reuseLabel: "true"},
			wantErr: true,
		},
		{
			name: "managed and reuse",
			labels: map[string]string{
				managedLabel: "true",
				reuseLabel:   "true",
			},
			wantErr: true,
		},
		{
			name: "managed reuse and generation",
			labels: map[string]string{
				managedLabel:  "true",
				reuseLabel:    "true",
				creationLabel: validCreation,
			},
			wantErr: false,
		},
		{
			name: "invalid generation",
			labels: map[string]string{
				managedLabel:  "true",
				reuseLabel:    "true",
				creationLabel: "not-a-generation",
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReuseOwned(&engineInfo{
				image:  "redis:7-alpine",
				labels: tc.labels,
			}, "redis:7-alpine", cfg)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("checkReuseOwned() error = %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckReuseLabelsRejectsMissingInspect(t *testing.T) {
	if err := checkReuseLabels(nil, &config{name: "myctr"}); err == nil {
		t.Fatal("checkReuseLabels(nil) succeeded")
	}
}

func TestCleanupFailedCreateRequiresMatchingGeneration(t *testing.T) {
	const creation = "0123456789abcdef"
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	cases := []struct {
		name       string
		labels     map[string]string
		reuse      bool
		wantDelete bool
	}{
		{
			name: "managed missing",
			labels: map[string]string{
				sessionLabel:  sessionID(),
				creationLabel: creation,
			},
		},
		{
			name: "generation missing",
			labels: map[string]string{
				managedLabel: "true",
				sessionLabel: sessionID(),
			},
		},
		{
			name: "generation empty",
			labels: map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: "",
			},
		},
		{
			name: "generation mismatched",
			labels: map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: "fedcba9876543210",
			},
		},
		{
			name: "reuse marker missing",
			labels: map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: creation,
			},
			reuse: true,
		},
		{
			name: "generation matches",
			labels: map[string]string{
				managedLabel:  "true",
				sessionLabel:  sessionID(),
				creationLabel: creation,
			},
			wantDelete: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &failRunRunner{
				fakeRunner:  newTestRunner(),
				runErr:      runErr,
				inspectJSON: inspectJSONWithLabels("myctr", tc.labels),
			}
			cfg := &config{
				runner:   runner,
				eng:      appleEngine{},
				name:     "myctr",
				reuse:    tc.reuse,
				creation: creation,
			}
			cleanupFailedCreate(context.Background(), cfg, runErr, runErr)
			if gotDelete := len(runner.deleted) > 0; gotDelete != tc.wantDelete {
				t.Fatalf("deleted = %v, want delete: %v", runner.deleted, tc.wantDelete)
			}
		})
	}
}

func inspectJSONWithLabels(name string, labels map[string]string) string {
	return inspectJSONWithStateAndLabels(name, "created", "redis:7-alpine", labels)
}

func inspectJSONWithStateAndLabels(name, state, image string, labels map[string]string) string {
	labelJSON, _ := json.Marshal(labels)
	return fmt.Sprintf(`[
  {"id": %q, "configuration": {"id": %q, "image": {"reference": %q}, "labels": %s}, "status": {"state": %q, "networks": []}}
]`, name, name, image, labelJSON, state)
}

func TestReuseDoesNotAdoptOrDeleteWithoutOwnershipLabels(t *testing.T) {
	const validCreation = "0123456789abcdef"
	cases := []struct {
		name        string
		state       State
		labels      map[string]string
		wantErr     bool
		wantDeleted bool
		wantCreated bool
	}{
		{
			name:    "running reuse only",
			state:   StateRunning,
			labels:  map[string]string{reuseLabel: "true"},
			wantErr: true,
		},
		{
			name:    "running without generation",
			state:   StateRunning,
			labels:  map[string]string{managedLabel: "true", reuseLabel: "true"},
			wantErr: true,
		},
		{
			name:    "stopped reuse only",
			state:   StateStopped,
			labels:  map[string]string{reuseLabel: "true"},
			wantErr: true,
		},
		{
			name:    "stopped without generation",
			state:   StateStopped,
			labels:  map[string]string{managedLabel: "true", reuseLabel: "true"},
			wantErr: true,
		},
		{
			name:  "running owned",
			state: StateRunning,
			labels: map[string]string{
				managedLabel:  "true",
				reuseLabel:    "true",
				creationLabel: validCreation,
			},
		},
		{
			name:  "stopped owned",
			state: StateStopped,
			labels: map[string]string{
				managedLabel:  "true",
				reuseLabel:    "true",
				creationLabel: validCreation,
			},
			wantDeleted: true,
			wantCreated: true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &reuseOwnershipRunner{
				fakeRunner: newTestRunner(),
				state:      tc.state,
				labels:     tc.labels,
			}
			runner.imagePresent = true
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName(fmt.Sprintf("owned-%d", i)), WithReuse(),
				withRunner(runner), withEngine(appleEngine{}))
			if tc.wantErr {
				if err == nil {
					t.Fatal("Run succeeded for an unowned container")
				}
				if ctr != nil {
					t.Fatalf("Run returned container for an unowned container: %+v", ctr)
				}
			} else if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := runner.deleted; got != tc.wantDeleted {
				t.Errorf("deleted = %v, want %v", got, tc.wantDeleted)
			}
			if got := runner.created; got != tc.wantCreated {
				t.Errorf("created = %v, want %v", got, tc.wantCreated)
			}
		})
	}
}

type reuseOwnershipRunner struct {
	*fakeRunner
	state  State
	labels map[string]string

	deleted bool
	created bool
}

func (r *reuseOwnershipRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		deleted, created := r.deleted, r.created
		r.mu.Unlock()
		if created {
			state := StateRunning
			return []byte(inspectJSONWithStateAndLabels(args[len(args)-1], string(state), "redis:7-alpine", r.labels)), nil, nil
		}
		if deleted {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
		}
		state := r.state
		return []byte(inspectJSONWithStateAndLabels(args[len(args)-1], string(state), "redis:7-alpine", r.labels)), nil, nil
	case "delete":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = true
		r.mu.Unlock()
		return nil, nil, nil
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.created = true
		r.mu.Unlock()
		return []byte("myctr\n"), nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestReuseDeleteNotFoundRemainsIdempotent(t *testing.T) {
	labels := map[string]string{
		managedLabel:  "true",
		reuseLabel:    "true",
		creationLabel: "0123456789abcdef",
	}
	r := &inspectErrorRunner{stderr: `inspect failed: not found: "shared"`}
	cfg := &config{runner: r, eng: appleEngine{}, name: "shared"}
	if err := deleteStoppedReuse(context.Background(), cfg, &engineInfo{state: StateStopped, labels: labels}); err != nil {
		t.Fatalf("deleteStoppedReuse: %v", err)
	}
	if r.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want 0 for a missing container", r.deleteCalls)
	}
}

func TestCleanupFailedCreateNotFoundRemainsIdempotent(t *testing.T) {
	r := &inspectErrorRunner{stderr: `inspect failed: not found: "myctr"`}
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	cfg := &config{
		runner:   r,
		eng:      appleEngine{},
		name:     "myctr",
		creation: "0123456789abcdef",
	}
	cleanupFailedCreate(context.Background(), cfg, runErr, runErr)
	if r.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want 0 for a missing container", r.deleteCalls)
	}
}
