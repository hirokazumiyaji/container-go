package container

import (
	"errors"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ErrCleanupFailed identifies a *CleanupError: the container left behind by a
// failed operation could not be removed. Callers can use errors.Is on a
// returned error to detect the whole class without knowing the container.
var ErrCleanupFailed = errors.New("container cleanup failed")

// CleanupError reports that an operation failed and the container it created
// could not be removed. It joins the cleanup failure onto the operation's
// error, so a caller can tell "the command failed and the container is gone"
// from "the command failed and the container is still running", and can reach
// the cleanup failure with errors.As or errors.Is.
//
// The operation error is whatever classification produced. Note that when the
// backend is unreachable, cli.Classify summarizes the underlying CLI failure as
// text, so the original *cli.CLIError may be present only as a message. The
// cleanup failure is always present as a structured error.
type CleanupError struct {
	// Container is the name or immutable ID of the container left behind.
	Container string
	// Err is the cleanup failure that left it behind.
	Err error
}

func (e *CleanupError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return "container " + e.Container + " left behind: " + ErrCleanupFailed.Error()
	}
	return "container " + e.Container + " left behind: " + e.Err.Error()
}

// Is classifies every CleanupError as ErrCleanupFailed while Unwrap keeps its
// historical single-error result for callers that inspect the chain directly.
func (e *CleanupError) Is(target error) bool {
	return e != nil && target == ErrCleanupFailed
}

func (e *CleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *CleanupError) asRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**CleanupError)
	if !ok || e == nil {
		return false
	}
	if r == nil {
		r = cli.NewRedactor()
	}
	*ptr = &CleanupError{Container: r.Text(e.Container), Err: cli.WithRedactor(e.Err, r)}
	return true
}

// withCleanupError attaches a cleanup failure to the error that caused it. The
// original stays first in the chain, so errors.Is on the operation error still
// works, and the cleanup failure is appended rather than replacing it.
func withCleanupError(cause, cleanupErr error) error {
	if cleanupErr == nil {
		return cause
	}
	if cause == nil {
		return cleanupErr
	}
	return errors.Join(cause, cleanupErr)
}
