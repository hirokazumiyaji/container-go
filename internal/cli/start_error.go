package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// PermanentStartError reports deterministic failures that cannot recover by
// starting the same executable again. Transient resource errors wrapped in
// PathError remain retryable.
func PermanentStartError(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	return os.IsPermission(err) || os.IsNotExist(err) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENOEXEC) ||
		badExecutableStartError(err)
}
