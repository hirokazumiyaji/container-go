package container

import "errors"

// CleanupError reports that an operation failed and the container it created
// could not be removed. Both the original failure and the cleanup failure stay
// in the chain, so a caller can recover each with errors.Is or errors.As and
// tell "the command failed and the container is gone" from "the command failed
// and the container is still running".
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
