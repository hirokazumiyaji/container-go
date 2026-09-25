//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// processIdentity pins the original child with a pidfd. Unlike cmd.Process's
// numeric PID fallback, a pidfd remains tied to the original process across
// Wait and cannot be redirected to a recycled PID.
type processIdentity struct {
	pid   int
	pidfd int
}

func openProcessIdentity(process *os.Process) (stableProcessIdentity, error) {
	if process == nil {
		return nil, os.ErrProcessDone
	}
	fd, err := unix.PidfdOpen(process.Pid, 0)
	if err != nil {
		return nil, err
	}
	return processIdentity{pid: process.Pid, pidfd: fd}, nil
}

func (p processIdentity) active() (bool, error) {
	for {
		fds := []unix.PollFd{{Fd: int32(p.pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return false, nil
		}
		return false, fmt.Errorf("pidfd poll returned unexpected events %#x", fds[0].Revents)
	}
}

func (p processIdentity) stop() error {
	return unix.PidfdSendSignal(p.pidfd, unix.SIGSTOP, nil, 0)
}

func (p processIdentity) stopped() (bool, error) {
	deadline := time.Now().Add(processStoppedObservationWindow)
	for {
		var info unix.Siginfo
		err := unix.Waitid(
			unix.P_PIDFD,
			p.pidfd,
			&info,
			unix.WSTOPPED|unix.WNOWAIT|unix.WNOHANG,
			nil,
		)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// CLD_TRAPPED and CLD_STOPPED are the two stopped states. WNOWAIT
		// leaves the state available to the lifecycle's sole Wait call.
		if info.Code == 4 || info.Code == 5 {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func (p processIdentity) kill() error {
	return unix.PidfdSendSignal(p.pidfd, unix.SIGKILL, nil, 0)
}

func (p processIdentity) groupID() (int, bool) {
	pgid, err := unix.Getpgid(p.pid)
	if err != nil {
		return 0, false
	}
	return pgid, true
}

func (p processIdentity) close() {
	if p.pidfd > 0 {
		_ = unix.Close(p.pidfd)
	}
}

// A pidfd cannot be recreated safely from a bare PID here. If pidfd_open was
// unavailable at attachment time, the retained os.Process fallback therefore
// declines group signaling and uses its direct handle only.
func observeProcessStopped(*os.Process) (bool, error) {
	return false, errProcessStopNotObserved
}
