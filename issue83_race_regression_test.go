package container

import (
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestCreateRaceMissingIsAnchoredAppleRunNameRace(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "direct Apple race",
			err: &cli.CLIError{
				Binary: "container",
				Args:   []string{"run", "--detach", "--name", "myctr"},
				Stderr: `Error: container with ID myctr not found`,
			},
			want: true,
		},
		{
			name: "quoted Apple race",
			err: &cli.CLIError{
				Binary: "container",
				Args:   []string{"run", "--name", "myctr"},
				Stderr: `Error: container with ID "myctr" not found`,
			},
			want: true,
		},
		{
			name: "wrapped Apple race",
			err: &cli.CLIError{
				Binary: "container",
				Args:   []string{"run", "--detach", "--name", "myctr"},
				Stderr: `Error: failed to bootstrap container: container with ID myctr not found`,
			},
			want: true,
		},
		{
			name: "generic not found",
			err: &cli.CLIError{
				Binary: "container",
				Args:   []string{"run", "--name", "myctr"},
				Stderr: `Error: container not found`,
			},
		},
		{
			name: "different name",
			err: &cli.CLIError{
				Binary: "container",
				Args:   []string{"run", "--name", "myctr"},
				Stderr: `Error: container with ID other not found`,
			},
		},
		{
			name: "wrong command",
			err: &cli.CLIError{
				Binary: "container",
				Args:   []string{"exec", "myctr", "true"},
				Stderr: `Error: container with ID myctr not found`,
			},
		},
		{
			name: "wrong backend",
			err: &cli.CLIError{
				Binary: "docker",
				Args:   []string{"run", "--name", "myctr"},
				Stderr: `Error: container with ID myctr not found`,
			},
		},
		{
			name: "missing backend identity",
			err: &cli.CLIError{
				Args:   []string{"run", "--name", "myctr"},
				Stderr: `Error: container with ID myctr not found`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createRaceMissing(tc.err); got != tc.want {
				t.Fatalf("createRaceMissing() = %v, want %v", got, tc.want)
			}
		})
	}
}
