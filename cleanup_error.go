package container

import "errors"

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
	return "container " + e.Container + " left behind: " + e.Err.Error()
}

func (e *CleanupError) Unwrap() error { return e.Err }

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

// leftBehind wraps a cleanup failure as a *CleanupError naming the container
// left behind. A nil cleanup error stays nil so callers can pass it straight
// to withCleanupError.
func leftBehind(name string, cleanupErr error) error {
	if cleanupErr == nil {
		return nil
	}
	return &CleanupError{Container: name, Err: cleanupErr}
}
