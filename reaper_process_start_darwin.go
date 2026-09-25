//go:build darwin

package container

import (
	"context"
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

// Darwin exposes the process start timestamp in proc_bsdinfo. Unlike ps's
// stime (elapsed CPU time), this value is immutable for the life of a
// process. The offsets below match the 64-bit Darwin proc_bsdinfo layout;
// the kernel rejects a buffer with an incompatible size.
const (
	darwinProcInfoCallPIDInfo = 2
	darwinProcPIDTBSDInfo     = 3
	darwinProcBSDInfoSize     = 136
	darwinBSDInfoPIDOffset    = 12
	darwinBSDInfoStartOffset  = 120
)

func reaperProcessStartTime(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if pid <= 0 {
		return "", fmt.Errorf("reaper: invalid process pid %d", pid)
	}

	// Keep the buffer local and parse it by offset so this remains a pure Go
	// build (no cgo/libproc headers are needed for cross-compilation).
	info := make([]byte, darwinProcBSDInfoSize)
	result, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		uintptr(darwinProcInfoCallPIDInfo),
		uintptr(pid),
		uintptr(darwinProcPIDTBSDInfo),
		0,
		uintptr(unsafe.Pointer(&info[0])),
		uintptr(len(info)),
	)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if result == 0 {
		if errno != 0 {
			return "", errno
		}
		return "", syscall.ESRCH
	}
	if result < darwinProcBSDInfoSize {
		return "", fmt.Errorf("reaper: Darwin process identity response was %d bytes", result)
	}
	if got := binary.LittleEndian.Uint32(info[darwinBSDInfoPIDOffset:]); got != uint32(pid) {
		return "", fmt.Errorf("reaper: Darwin process identity PID = %d, want %d", got, pid)
	}
	seconds := binary.LittleEndian.Uint64(info[darwinBSDInfoStartOffset:])
	microseconds := binary.LittleEndian.Uint64(info[darwinBSDInfoStartOffset+8:])
	if seconds == 0 && microseconds == 0 {
		return "", fmt.Errorf("reaper: Darwin returned no process start time for pid %d", pid)
	}
	return fmt.Sprintf("%d.%09d", seconds, microseconds), nil
}
