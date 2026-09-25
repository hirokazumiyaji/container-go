package container

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package.
type CLIError = cli.CLIError

// CleanupError reports a primary operation failure together with a failure
// from automatic cleanup or verification of the retained container. Both
// causes remain available through errors.Is/errors.As.
type CleanupError struct {
	Err        error
	CleanupErr error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%v; cleanup failed: %v", e.Err, e.CleanupErr)
}

// Unwrap preserves both the operation and cleanup branches.
func (e *CleanupError) Unwrap() []error {
	return []error{e.Err, e.CleanupErr}
}

func withCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return &CleanupError{Err: err, CleanupErr: cleanupErr}
}

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts.
var ErrPortNotExposed = errors.New("port not declared via WithExposedPorts")

// ErrImageNotFound reports that an image is not in the backend's local
// store. Run returns it when the pull policy is PullNever and the image
// is absent.
var ErrImageNotFound = errors.New("image not found in local store")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, and Logs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that an operation refused to use or
// delete a live container because its creation generation or backend
// identity no longer matches the handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// ErrNameLockCompatibility reports that a name-addressed operation could
// not establish every historical lock namespace required during migration.
var ErrNameLockCompatibility = errors.New("name lock compatibility")

// isNotFound reports whether a CLI failure means the container does not
// exist. Matching substrings live on each engine (see engine_*.go).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	switch {
	case strings.TrimSpace(cliErr.Binary) == "":
		// An empty executable is the historical fake-runner default;
		// retain both backend wordings only for that ambiguous legacy
		// form. Explicit executable names remain backend-scoped.
		return appleEngine{}.containerMissing(err) || dockerEngine{}.containerMissing(err)
	case cliBinaryMatches(cliErr.Binary, "docker"):
		return dockerEngine{}.containerMissing(err)
	case cliBinaryMatches(cliErr.Binary, "container"):
		return appleEngine{}.containerMissing(err)
	default:
		return false
	}
}

func cliBinaryMatches(got, want string) bool {
	got = strings.ToLower(strings.TrimSpace(got))
	want = strings.ToLower(strings.TrimSpace(want))
	if got == "" {
		return want == "container"
	}
	if i := strings.LastIndexAny(got, `/\\`); i >= 0 {
		got = got[i+1:]
	}
	return got == want
}

// isDeleteNotFound is the backend/command/target-scoped form used for
// idempotent deletion. A generic "not found" substring from another
// operation or target is not evidence that this delete removed a live
// container.
func isDeleteNotFound(eng engine, target string, err error) bool {
	if eng == nil || target == "" {
		return false
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	expected := eng.deleteArgs(target)
	if len(cliErr.Args) != len(expected) {
		return false
	}
	for i := range expected {
		if cliErr.Args[i] != expected[i] {
			return false
		}
	}
	if !cliBinaryMatches(cliErr.Binary, eng.binary()) {
		return false
	}
	if eng.name() == "docker" && cliErr.Binary == "" {
		return false
	}
	if !deleteStderrTargetMatches(eng, target, cliErr.Stderr) {
		return false
	}
	switch eng.name() {
	case "apple":
		return appleEngine{}.containerMissing(err)
	case "docker":
		return dockerEngine{}.containerMissing(err)
	default:
		return false
	}
}

func deleteStderrTargetMatches(eng engine, target, stderr string) bool {
	if eng == nil || target == "" {
		return false
	}
	want := strings.TrimSpace(target)
	for _, raw := range strings.Split(strings.ToLower(stderr), "\n") {
		line := normalizeDeleteStderrLine(raw)
		switch eng.name() {
		case "apple":
			if appleDeleteMissingLine(line, want) {
				return true
			}
		case "docker":
			if dockerDeleteMissingLine(line, want) {
				return true
			}
		}
	}
	return false
}

func normalizeDeleteStderrLine(line string) string {
	line = strings.TrimSpace(line)
	for {
		changed := false
		for _, prefix := range []string{
			"error response from daemon: ",
			"error: ",
			"container: ",
			"docker: ",
		} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				changed = true
				break
			}
		}
		if !changed {
			return line
		}
	}
}

func appleDeleteMissingLine(line, want string) bool {
	for _, wrapper := range []string{
		"failed to delete container: ",
		"delete failed: ",
	} {
		if strings.HasPrefix(line, wrapper) {
			line = strings.TrimSpace(strings.TrimPrefix(line, wrapper))
			break
		}
	}
	if rest, ok := strings.CutPrefix(line, "container not found:"); ok {
		return deleteTargetTokenMatches(rest, want)
	}
	if rest, ok := strings.CutPrefix(line, "container with id "); ok {
		return deleteIDNotFoundMatches(rest, want)
	}
	if rest, ok := strings.CutPrefix(line, "not found:"); ok {
		return deleteTargetTokenMatches(rest, want)
	}
	return false
}

func dockerDeleteMissingLine(line, want string) bool {
	for _, prefix := range []string{"no such container:", "no such object:"} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return deleteTargetTokenMatches(rest, want)
		}
	}
	return false
}

func deleteIDNotFoundMatches(rest, want string) bool {
	rest = strings.TrimSpace(rest)
	const suffix = " not found"
	if !strings.HasSuffix(rest, suffix) {
		return false
	}
	return deleteTargetTokenMatches(strings.TrimSpace(strings.TrimSuffix(rest, suffix)), want)
}

func deleteTargetTokenMatches(rest, want string) bool {
	rest = strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), "\"'"))
	return rest == want
}

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
