//go:build darwin

package container

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"
)

// Darwin exposes the process start timestamp in proc_bsdinfo. Unlike ps's
// stime (elapsed CPU time), this value is immutable for the life of a
// process. Keep this structure in the same order as Darwin's
// <sys/proc_info.h>; unsafe.Offsetof then makes the identity layout explicit
// instead of relying on unexplained byte offsets.
type darwinProcBSDInfo struct {
	flags             uint32
	status            uint32
	xstatus           uint32
	pid               uint32
	ppid              uint32
	uid               uint32
	gid               uint32
	ruid              uint32
	rgid              uint32
	svuid             uint32
	svgid             uint32
	reserved          uint32
	comm              [16]byte
	name              [32]byte
	nfiles            uint32
	pgid              uint32
	jobControl        uint32
	terminalDevice    uint32
	terminalPGID      uint32
	nice              int32
	startSeconds      uint64
	startMicroseconds uint64
}

const (
	darwinProcInfoCallPIDInfo = 2
	darwinProcPIDTBSDInfo     = 3
)

func reaperProcessStartTime(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if pid <= 0 {
		return "", fmt.Errorf("reaper: invalid process pid %d", pid)
	}

	// The pure-Go layout is used directly as the kernel buffer, so this
	// remains cross-compilable without cgo or libproc headers.
	var info darwinProcBSDInfo
	result, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		uintptr(darwinProcInfoCallPIDInfo),
		uintptr(pid),
		uintptr(darwinProcPIDTBSDInfo),
		0,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
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
	if result < unsafe.Sizeof(info) {
		return "", fmt.Errorf("reaper: Darwin process identity response was %d bytes", result)
	}
	if info.pid != uint32(pid) {
		return "", fmt.Errorf("reaper: Darwin process identity PID = %d, want %d", info.pid, pid)
	}
	if info.startSeconds == 0 && info.startMicroseconds == 0 {
		return "", fmt.Errorf("reaper: Darwin returned no process start time for pid %d", pid)
	}
	return fmt.Sprintf("%d.%09d", info.startSeconds, info.startMicroseconds), nil
}
