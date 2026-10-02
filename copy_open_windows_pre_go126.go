//go:build windows && !go1.26

package container

// Go 1.23 through 1.25 ignore FILE_FLAG_* values in syscall.Open, and
// os.OpenFile therefore silently follows reparse points and creates a
// synchronous handle. Keep the copy-out path closed on those toolchains.
const windowsCopyFileOpenSupported = false

const windowsCopyFileOpenFlags = 0
