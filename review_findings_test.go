package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestAppleTypedNotFoundIsOperationSpecific(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		stderr    string
		want      bool
	}{
		{
			name:      "exec get failed",
			operation: "exec",
			stderr:    `Error: notFound: "get failed: container myctr not found"`,
			want:      true,
		},
		{
			name:      "exec generic inspect form",
			operation: "exec",
			stderr:    `Error: notFound: "container not found: myctr"`,
		},
		{
			name:      "inspect exec form",
			operation: "inspect",
			stderr:    `Error: notFound: "get failed: container myctr not found"`,
		},
		{
			name:      "inspect lifecycle form",
			operation: "inspect",
			stderr:    `Error: notFound: "container with id myctr not found"`,
		},
		{
			name:      "inspect generic form",
			operation: "inspect",
			stderr:    `Error: notFound: "container not found: myctr"`,
			want:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{tc.operation, "myctr"}
			if tc.operation == "exec" {
				args = append(args, "true")
			}
			err := &cli.CLIError{Binary: "container", Args: args, Stderr: tc.stderr}
			if got := (appleEngine{}).containerMissing(err); got != tc.want {
				t.Fatalf("containerMissing() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAppleTypedNotFoundParsesNestedCauses(t *testing.T) {
	err := &cli.CLIError{
		Binary: "container",
		Args:   []string{"inspect", "myctr"},
		Stderr: `Error: internalError: "outer" (cause: "internalError: \"middle\" (cause: \"notFound: \\\"container not found: myctr\\\"\")")`,
	}
	if !(appleEngine{}).containerMissing(err) {
		t.Fatal("nested typed not-found was not recognized")
	}
}

func TestAppleLogsDashNTargetExtraction(t *testing.T) {
	err := &cli.CLIError{
		Binary: "container",
		Args:   []string{"logs", "-n", "1000", "myctr"},
		Stderr: "Error: get failed: container myctr not found",
	}
	if !(appleEngine{}).containerMissing(err) {
		t.Fatal("logs -n target was extracted from the option value")
	}
}

type reuseRollbackReviewRunner struct {
	creation string
	inspect  int
	deletes  int
	copyErr  error
}

func (r *reuseRollbackReviewRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		if args[1] == "inspect" {
			return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
		}
		return nil, nil, nil
	case "run":
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = value
				}
			}
		}
		return []byte("shared\n"), nil, nil
	case "inspect":
		r.inspect++
		if r.creation == "" {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `Error: container not found: "shared"`}
		}
		return []byte(fmt.Sprintf(`[{"id":"shared","configuration":{"id":"shared","image":{"reference":"redis:7-alpine"},"labels":{%q:"true",%q:"true",%q:%q}},"status":{"state":"running","networks":[]}}]`, managedLabel, reuseLabel, creationLabel, r.creation)), nil, nil
	case "cp":
		return nil, nil, r.copyErr
	case "delete", "rm":
		r.deletes++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestReuseRollbackDoesNotDeleteRunningGeneration(t *testing.T) {
	host := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(host, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &reuseRollbackReviewRunner{copyErr: errors.New("copy failed")}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithFiles(File{HostPath: host, ContainerPath: "/tmp/input"}),
		withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "copy failed") {
		t.Fatalf("Run error = %v, want copy failure", err)
	}
	if r.deletes != 0 {
		t.Fatalf("automatic reuse rollback deleted %d running generations", r.deletes)
	}
}

func TestDeleteStoppedReuseFailsClosedWithoutOwnershipLabels(t *testing.T) {
	r := &reuseRollbackReviewRunner{}
	info := &engineInfo{state: StateStopped, labels: map[string]string{reuseLabel: "true"}}
	if err := deleteStoppedReuse(context.Background(), &config{runner: r, eng: appleEngine{}, name: "shared"}, info); err == nil {
		t.Fatal("deleteStoppedReuse accepted missing managed/creation labels")
	}
	if r.deletes != 0 {
		t.Fatalf("delete calls = %d, want 0 for unowned container", r.deletes)
	}
}

type applePruneReviewRunner struct {
	listJSON    string
	inspectJSON string
	deletes     []string
}

func (r *applePruneReviewRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ls":
		return []byte(r.listJSON), nil, nil
	case "inspect":
		return []byte(r.inspectJSON), nil, nil
	case "delete":
		r.deletes = append(r.deletes, args[len(args)-1])
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestApplePruneRetainsRunningReplacement(t *testing.T) {
	const creation = "0123456789abcdef"
	r := &applePruneReviewRunner{
		listJSON:    fmt.Sprintf(`[{"id":"shared","configuration":{"labels":{%q:"true",%q:%q}},"status":{"state":"stopped","networks":[]}}]`, managedLabel, creationLabel, creation),
		inspectJSON: fmt.Sprintf(`[{"id":"shared","configuration":{"labels":{%q:"true",%q:%q}},"status":{"state":"running","networks":[]}}]`, managedLabel, creationLabel, creation),
	}
	removed, err := pruneWith(context.Background(), r, appleEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 0 || len(r.deletes) != 0 {
		t.Fatalf("removed=%v deletes=%v, want running replacement retained", removed, r.deletes)
	}
}

func TestDockerPruneRequestsFullIDsWithoutQuiet(t *testing.T) {
	args := (dockerEngine{}).listArgs()
	if !reviewContains(args, "--no-trunc") || reviewContains(args, "--quiet") {
		t.Fatalf("listArgs = %v, want --no-trunc and no --quiet", args)
	}
	if _, err := (dockerEngine{}).parseStoppedManaged([]byte("short-name\n")); err == nil {
		t.Fatal("Docker prune accepted a non-full container ID")
	}
}

func reviewContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
