package container

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func readApple13Fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func appleFixtureError(t *testing.T, name, operation, target string) error {
	t.Helper()
	args := []string{operation, target}
	if operation == "run" {
		args = []string{"run", "--name", target}
	}
	return &cli.CLIError{
		Binary: "container", Args: args, ExitCode: 1,
		Stderr: readApple13Fixture(t, name),
	}
}

func TestApple13TypedErrorsMatchTheirOperations(t *testing.T) {
	cases := []struct {
		name      string
		fixture   string
		operation string
		match     func(error) bool
	}{
		{name: "inspect", fixture: "apple_1.3_inspect_notfound.txt", operation: "inspect", match: func(err error) bool {
			return (appleEngine{}).containerMissing(err)
		}},
		{name: "exec internal cause", fixture: "apple_1.3_exec_internal_cause.txt", operation: "exec", match: func(err error) bool {
			return (appleEngine{}).containerMissing(err)
		}},
		{name: "stop", fixture: "apple_1.3_stop_notfound.txt", operation: "stop", match: func(err error) bool {
			return (appleEngine{}).containerMissing(err)
		}},
		{name: "delete internal cause", fixture: "apple_1.3_delete_internal_cause.txt", operation: "delete", match: func(err error) bool {
			return (appleEngine{}).containerMissing(err)
		}},
		{name: "logs internal cause", fixture: "apple_1.3_logs_internal_cause.txt", operation: "logs", match: func(err error) bool {
			return (appleEngine{}).containerMissing(err)
		}},
		{name: "image inspect", fixture: "apple_1.3_image_notfound.txt", operation: "image inspect", match: func(err error) bool {
			return (appleEngine{}).imageMissingForTarget(err, "redis:7-alpine")
		}},
		{name: "run conflict", fixture: "apple_1.3_exists.txt", operation: "run", match: func(err error) bool {
			return (appleEngine{}).nameConflictForTarget(err, "myctr")
		}},
		{name: "create race cause", fixture: "apple_1.3_run_race_internal_cause.txt", operation: "run", match: func(err error) bool {
			return createRaceMissingForTarget(err, "myctr")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := appleFixtureError(t, tc.fixture, tc.operation, "myctr")
			if tc.operation == "image inspect" {
				err = &cli.CLIError{
					Binary: "container", Args: []string{"image", "inspect", "redis:7-alpine"}, ExitCode: 1,
					Stderr: readApple13Fixture(t, tc.fixture),
				}
			}
			if !tc.match(err) {
				t.Fatalf("typed Apple 1.3 error was not matched: %v", err)
			}
		})
	}
}

func TestApple13ApplicationPrefixedErrorsAreRejected(t *testing.T) {
	fixture := readApple13Fixture(t, "apple_1.3_application_prefixed.txt")
	cases := []struct {
		name string
		err  *cli.CLIError
		want bool
	}{
		{
			name: "inspect",
			err:  &cli.CLIError{Binary: "container", Args: []string{"inspect", "myctr"}, Stderr: fixture},
		},
		{
			name: "image",
			err:  &cli.CLIError{Binary: "container", Args: []string{"image", "inspect", "redis:7-alpine"}, Stderr: fixture},
			want: false,
		},
		{
			name: "run",
			err:  &cli.CLIError{Binary: "container", Args: []string{"run", "--name", "myctr"}, Stderr: fixture},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			switch tc.name {
			case "inspect":
				got = (appleEngine{}).containerMissing(tc.err)
			case "image":
				got = (appleEngine{}).imageMissingForTarget(tc.err, "redis:7-alpine")
			case "run":
				got = (appleEngine{}).nameConflictForTarget(tc.err, "myctr") || createRaceMissingForTarget(tc.err, "myctr")
			}
			if got != tc.want {
				t.Fatalf("application-prefixed error matched = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestApple13TypedImageTargetKeepsCaseSensitivity(t *testing.T) {
	err := &cli.CLIError{
		Binary: "container", Args: []string{"image", "inspect", "redis:ALPINE"}, ExitCode: 1,
		Stderr: "Error: notFound: \"image not found: redis:ALPINE\"",
	}
	if !(appleEngine{}).imageMissingForTarget(err, "redis:ALPINE") {
		t.Fatal("typed image error did not match exact target case")
	}
	if (appleEngine{}).imageMissingForTarget(err, "redis:alpine") {
		t.Fatal("typed image error matched a case-mismatched target")
	}
}

func TestAppleClassifiersIgnoreUnrelatedJoinedTargetBranches(t *testing.T) {
	otherConflict := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "other"}, ExitCode: 1,
		Stderr: "Error: exists: \"container with id other already exists\"",
	}
	current := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	joined := errors.Join(otherConflict, current)
	if (appleEngine{}).nameConflictForTarget(joined, "myctr") {
		t.Fatal("unrelated name-conflict branch was selected")
	}

	otherImage := &cli.CLIError{
		Binary: "container", Args: []string{"image", "inspect", "other"}, ExitCode: 1,
		Stderr: "Error: notFound: \"image not found: other\"",
	}
	currentImage := &cli.CLIError{
		Binary: "container", Args: []string{"image", "inspect", "redis:7-alpine"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	if (appleEngine{}).imageMissingForTarget(errors.Join(otherImage, currentImage), "redis:7-alpine") {
		t.Fatal("unrelated image-missing branch was selected")
	}

	otherRace := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "other"}, ExitCode: 1,
		Stderr: "Error: notFound: \"container with ID other not found\"",
	}
	if createRaceMissingForTarget(errors.Join(otherRace, current), "myctr") {
		t.Fatal("unrelated create-race branch was selected")
	}
}

func TestAppleClassifiersStillMatchCurrentTargetBranch(t *testing.T) {
	err := errors.Join(
		&cli.CLIError{Binary: "container", Args: []string{"run", "--name", "other"}, Stderr: "Error: exists: \"container with id other already exists\""},
		&cli.CLIError{Binary: "container", Args: []string{"run", "--name", "myctr"}, Stderr: "Error: exists: \"container with id myctr already exists\""},
	)
	if !(appleEngine{}).nameConflictForTarget(err, "myctr") {
		t.Fatal("current target name-conflict branch was not selected")
	}
}
