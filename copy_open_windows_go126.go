//go:build windows && go1.26

package container

import "syscall"

// Go 1.26 added pass-through for the Windows FILE_FLAG_* values supplied
// to os.OpenFile, and uses FILE_FLAG_OVERLAPPED to register the handle
// with the runtime poller.
const windowsCopyFileOpenSupported = true

const windowsCopyFileOpenFlags = syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.FILE_FLAG_OVERLAPPED
