package container

import (
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestDeleteNotFoundIsBackendCommandAndTargetScoped(t *testing.T) {
	appleErr := &cli.CLIError{
		Binary: "container",
		Args:   []string{"delete", "--force", "myctr"},
		Stderr: `container not found: myctr`,
	}
	if !isDeleteNotFound(appleEngine{}, "myctr", appleErr) {
		t.Fatal("matching Apple delete was not idempotent")
	}
	upperErr := &cli.CLIError{
		Binary: "container",
		Args:   []string{"delete", "--force", "MyCtr"},
		Stderr: "Container Not Found: MYCTR",
	}
	if !isNotFound(upperErr) || !isDeleteNotFound(appleEngine{}, "MyCtr", upperErr) {
		t.Fatal("uppercase Apple name was not classified case-insensitively")
	}
	for _, tc := range []struct {
		name string
		eng  engine
		id   string
		err  error
	}{
		{
			name: "wrong target",
			eng:  appleEngine{},
			id:   "other",
			err:  appleErr,
		},
		{
			name: "stderr names another target",
			eng:  appleEngine{},
			id:   "myctr",
			err:  &cli.CLIError{Binary: "container", Args: []string{"delete", "--force", "myctr"}, Stderr: "container not found: other"},
		},
		{
			name: "wrong command",
			eng:  appleEngine{},
			id:   "myctr",
			err:  &cli.CLIError{Binary: "container", Args: []string{"inspect", "myctr"}, Stderr: "container not found: myctr"},
		},
		{
			name: "wrong backend",
			eng:  appleEngine{},
			id:   "myctr",
			err:  &cli.CLIError{Binary: "docker", Args: []string{"rm", "--force", strings.Repeat("a", 64)}, Stderr: "No such container"},
		},
		{
			name: "docker without binary identity",
			eng:  dockerEngine{},
			id:   strings.Repeat("a", 64),
			err:  &cli.CLIError{Args: []string{"rm", "--force", strings.Repeat("a", 64)}, Stderr: "No such container: " + strings.Repeat("a", 64)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if isDeleteNotFound(tc.eng, tc.id, tc.err) {
				t.Fatal("mismatched delete error was treated as idempotent")
			}
		})
	}
}
