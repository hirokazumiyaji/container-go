//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	pid   int
	pidfd int
}

func openProcessIdentity(pid int) (processIdentity, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{pid: pid, pidfd: fd}, nil
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

func (p processIdentity) groupID() (int, bool) {
	pgid, err := unix.Getpgid(p.pid)
	if err != nil {
		return 0, false
	}
	return pgid, true
}

func (p processIdentity) close() { _ = unix.Close(p.pidfd) }

func processGroupTerminationSupported() bool {
	identity, err := openProcessIdentity(os.Getpid())
	if err != nil {
		return false
	}
	defer identity.close()
	active, err := identity.active()
	return err == nil && active
}
