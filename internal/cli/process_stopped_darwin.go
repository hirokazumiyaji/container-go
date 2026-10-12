//go:build darwin

package cli

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const darwinWaitIDProcess = 0 // idtype_t.P_PID

func openProcessIdentity(process *os.Process) (stableProcessIdentity, error) {
	if process == nil {
		return nil, os.ErrProcessDone
	}
	return retainedProcessIdentity{process: process}, nil
}

// observeProcessStopped uses Darwin's waitid(2) with WNOWAIT. The numeric
// PID is used only after the retained os.Process accepted SIGSTOP, and the
// stopped state is left un-reaped for Cmd.Wait. If the process was reaped in
// the race, the caller receives false and takes the direct-handle fallback.
func observeProcessStopped(process *os.Process) (bool, error) {
	if process == nil {
		return false, os.ErrProcessDone
	}
	deadline := time.Now().Add(processStoppedObservationWindow)
	for {
		var info [128]byte
		_, _, errno := syscall.Syscall6(
			// x/sys/unix marks every Darwin syscall number deprecated and
			// ships no waitid wrapper for Darwin, so the observation needs
			// the raw entry point.
			unix.SYS_WAITID, //nolint:staticcheck // no libSystem wrapper exists for Darwin waitid
			uintptr(darwinWaitIDProcess),
			uintptr(process.Pid),
			uintptr(unsafe.Pointer(&info[0])),
			uintptr(unix.WSTOPPED|unix.WNOWAIT|unix.WNOHANG),
			0,
			0,
		)
		if errno != 0 {
			if errors.Is(errno, syscall.EINTR) {
				continue
			}
			if errors.Is(errno, syscall.ECHILD) || errors.Is(errno, syscall.ESRCH) {
				return false, nil
			}
			return false, errno
		}
		// Darwin's siginfo_t starts with signo, errno, and code as
		// 32-bit fields. CLD_TRAPPED and CLD_STOPPED are 4 and 5.
		code := int32(*(*uint32)(unsafe.Pointer(&info[8])))
		if code == 4 || code == 5 {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(time.Millisecond)
	}
}
