//go:build windows

package cli

import (
	"errors"
	"syscall"
)

func badExecutableStartError(err error) bool {
	// syscall does not export named constants for these Win32 codes:
	// 193 ERROR_BAD_EXE_FORMAT ("%1 is not a valid Win32 application")
	// 216 ERROR_EXE_MACHINE_TYPE_MISMATCH ("This version of %1 is not
	// compatible with the version of Windows you're running")
	// Writing a non-PE blob as .exe commonly surfaces as 216 on CI hosts.
	return errors.Is(err, syscall.Errno(193)) || errors.Is(err, syscall.Errno(216))
}
