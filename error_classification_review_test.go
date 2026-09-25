package container

import (
	"github.com/hirokazumiyaji/container-go/internal/cli"
	"testing"
)

func TestReviewDockerProbeEndpointAuthWords(t *testing.T) {
	args := []string{"version", "--format", "{{.Server.Version}}"}
	for _, text := range []string{
		"Cannot connect to the Docker daemon at unix:///Users/test/.docker/credential/docker.sock: connect: connection refused",
		"Cannot connect to the Docker daemon at tcp://forbidden.example:2376: connect: connection refused",
		"Cannot connect to the Docker daemon at ssh://unauthorized@example: connection refused",
	} {
		err := &cli.CLIError{Binary: "docker", Args: args, Stderr: text}
		if !dockerProbeUnavailable(err) {
			t.Errorf("false for %q", text)
		}
	}
	for _, text := range []string{
		"error during connect: invalid configuration for current context",
		"error during connect: authentication required",
		"error during connect: 403 forbidden",
		"error during connect: credential helper failed",
	} {
		err := &cli.CLIError{Binary: "docker", Args: args, Stderr: text}
		if dockerProbeUnavailable(err) {
			t.Errorf("true for %q", text)
		}
	}
}

func TestReviewAppleTypedNameConflict(t *testing.T) {
	for _, stderr := range []string{
		`Error: exists: "container with id myctr already exists"`,
		`Error: internalError: "failed to create container" (cause: "exists: "container with id myctr already exists"")`,
	} {
		err := &cli.CLIError{Binary: "container", Args: []string{"run", "--name", "myctr"}, Stderr: stderr}
		if !(appleEngine{}).nameConflict(err) {
			t.Errorf("typed name conflict was not recognized: %q", stderr)
		}
	}
}

func TestReviewCreateRaceMissingRecognizesTypedAppleID(t *testing.T) {
	for _, stderr := range []string{
		`Error: notFound: "container with id myctr not found"`,
		`Error: internalError: "failed to run container" (cause: "notFound: \"container with id myctr not found\"")`,
	} {
		err := &cli.CLIError{Binary: "container", Args: []string{"run", "--name", "myctr"}, Stderr: stderr}
		if !createRaceMissing(err) {
			t.Errorf("typed create race was not recognized: %q", stderr)
		}
	}
}

func TestReviewAppleTypedForms(t *testing.T) {
	cases := []struct {
		op, target, stderr string
		want               bool
	}{
		{"inspect", "myctr", `Error: notFound: "container not found: myctr"`, true},
		{"inspect", "myctr", `Error: internalError: "failed to list containers" (cause: "notFound: \"container not found: myctr\"")`, true},
		{"inspect", "myctr", `Error: internalError: "failed to list containers" (cause: "notFound: "container not found: myctr"")`, true},
		{"exec", "myctr", `Error: notFound: "get failed: container myctr not found"`, true},
		{"delete", "myctr", `Error: internalError: "failed to delete container" (cause: "notFound: \"container not found: myctr\"")`, true},
		{"logs", "myctr", `Error: internalError: "failed to get logs for container myctr" (cause: "notFound: \"container not found: myctr\"")`, true},
		{"logs", "myctr", `Error: invalidArgument: "failed to fetch container logs for myctr: notFound: "get failed: container myctr not found""`, true},
		{"inspect", "myctr", `Error: notFound: "get failed: container myctr not found"`, false},
		{"exec", "myctr", `Error: application: notFound: "container not found: myctr"`, false},
		{"inspect", "myctr", `Error: notFound: "container not found: other"`, false},
		{"inspect", "myctr", `Error: notFound: "container not found: myctr`, false},
	}
	for _, tc := range cases {
		args := []string{tc.op, tc.target}
		if tc.op == "exec" {
			args = append(args, "true")
		}
		err := &cli.CLIError{Binary: "container", Args: args, Stderr: tc.stderr}
		if got := (appleEngine{}).containerMissing(err); got != tc.want {
			t.Errorf("%s %q => %v want %v", tc.op, tc.stderr, got, tc.want)
		}
	}
}
