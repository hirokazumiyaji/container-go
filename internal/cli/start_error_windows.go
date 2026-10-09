//go:build windows

package cli

import (
	"errors"
	"syscall"
)

func badExecutableStartError(err error) bool {
	// ERROR_BAD_EXECUTABLE_FORMAT is 193 in the Windows system error
	// namespace. syscall does not export a named constant for it.
	return errors.Is(err, syscall.Errno(193))
}
