package container

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package.
type CLIError = cli.CLIError

// CleanupError reports an operation failure together with a failure from
// automatic cleanup or verification of a retained container. Both errors
// remain in the returned chain through Err and CleanupErr.
type CleanupError struct {
	Err        error
	CleanupErr error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%v; cleanup failed: %v", e.Err, e.CleanupErr)
}

// Unwrap returns both the operation and cleanup errors.
func (e *CleanupError) Unwrap() []error {
	return []error{e.Err, e.CleanupErr}
}

func withCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return &CleanupError{Err: err, CleanupErr: cleanupErr}
}

// primaryOperationError returns only the operation branch of a
// CleanupError. Retry classifiers must not read CleanupErr, which may
// hold an unrelated backend failure that happens to look retryable.
func primaryOperationError(err error) error {
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		return cleanupErr.Err
	}
	return err
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

// ErrGenerationReplaced reports that Terminate refused to delete because
// the live container's creation label no longer matches this handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// isNotFound reports whether a CLI failure means the container does not
// exist. A backend liveness wrapper vetoes the classification: when the
// CLI could not be reached, nested "not found" text describes an
// unreachable backend, not an absent container. Matching substrings live
// on each engine (see engine_*.go).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	if errors.Is(err, ErrSystemNotRunning) {
		return false
	}
	return (appleEngine{}).containerMissing(err) || (dockerEngine{}).containerMissing(err)
}

// isNotFoundFor is the operation- and target-scoped form used by
// lifecycle paths. Absence is only evidence for the command that
// addressed this container: another operation's or another target's
// "not found" text must not suppress a real failure, and a wrapped
// ErrSystemNotRunning never counts as absence.
func isNotFoundFor(eng engine, target string, err error) bool {
	return notFoundForOps(eng, target, err, "inspect", "delete", "rm", "stop", "logs", "exec")
}

// isDeleteNotFound reports whether err is a delete of exactly target
// that the backend answered with a not-found. It is the only form that
// makes an idempotent delete a success.
func isDeleteNotFound(eng engine, target string, err error) bool {
	return notFoundForOps(eng, target, err, "delete", "rm")
}

// notFoundForOps reports whether err is a not-found for target raised by
// one of ops on this backend.
func notFoundForOps(eng engine, target string, err error, ops ...string) bool {
	if err == nil || target == "" || errors.Is(err, ErrSystemNotRunning) {
		return false
	}
	cliErr, ok := lifecycleCLIError(eng, err, ops...)
	if !ok {
		return false
	}
	if cliErr.Args[0] == "exec" {
		return execTarget(cliErr.Args) == target
	}
	return strings.EqualFold(cliErr.Args[len(cliErr.Args)-1], target)
}

// lifecycleCLIError returns the CLI failure this backend raised for one
// of ops, ignoring failures raised for other commands (an image pull or
// a liveness probe, for example) and for the other backend. Without a
// backend there is no way to attribute the wording, so it reports none.
func lifecycleCLIError(eng engine, err error, ops ...string) (*cli.CLIError, bool) {
	if eng == nil {
		return nil, false
	}
	var cliErrs []*cli.CLIError
	collectCLIErrors(err, &cliErrs)
	for _, cliErr := range cliErrs {
		if len(cliErr.Args) == 0 || !slices.Contains(ops, cliErr.Args[0]) {
			continue
		}
		if !cliBinaryMatches(cliErr.Binary, eng.binary()) {
			continue
		}
		switch eng.name() {
		case "docker":
			if !(dockerEngine{}).containerMissing(cliErr) {
				continue
			}
		default:
			if !(appleEngine{}).containerMissing(cliErr) {
				continue
			}
		}
		return cliErr, true
	}
	return nil, false
}

func execTarget(args []string) string {
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--env-file", "--user", "--workdir":
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		return args[i]
	}
	return ""
}

// cliBinaryMatches compares a reported executable with the engine's
// binary, tolerating absolute paths. An empty reported binary is the
// historical CLIError default, which predates named executables, so it
// is accepted and the backend's own stderr wording decides instead.
func cliBinaryMatches(got, want string) bool {
	got = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(got)), ".exe")
	if i := strings.LastIndexAny(got, `/\`); i >= 0 {
		got = got[i+1:]
	}
	return got == "" || got == strings.ToLower(want)
}

// collectCLIErrors gathers CLI failures through both single- and
// multi-error wrappers, including the backend liveness wrapper that
// keeps the original command failure.
func collectCLIErrors(err error, out *[]*cli.CLIError) {
	if err == nil {
		return
	}
	if cliErr, ok := err.(*cli.CLIError); ok {
		*out = append(*out, cliErr)
		return
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			collectCLIErrors(child, out)
		}
	case interface{ Unwrap() error }:
		collectCLIErrors(e.Unwrap(), out)
	}
}

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
