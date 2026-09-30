//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const copySourceOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK

func copySourceFileIdentity(file *os.File) (copySourceIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return copySourceIdentity{}, err
	}
	return copySourceIdentity{primary: uint64(stat.Dev), secondary: uint64(stat.Ino)}, nil
}

func sameCopySourceIdentity(first, second copySourceIdentity) bool {
	return first.primary == second.primary && first.secondary == second.secondary
}

// openCopySource opens the final component without following links. O_NONBLOCK
// keeps a raced FIFO from blocking before the handle can be validated.
func openCopySource(path string) (*os.File, bool, error) {
	fd, err := unix.Open(path, copySourceOpenFlags, 0)
	if err != nil {
		return nil, false, err
	}
	return os.NewFile(uintptr(fd), path), false, nil
}

// openCopySourceAt opens one directory entry relative to the already-open
// parent handle, so a renamed parent cannot redirect the child lookup.
func openCopySourceAt(parent *os.File, name string) (*os.File, bool, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, copySourceOpenFlags, 0)
	if err != nil {
		return nil, false, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name)), false, nil
}

func isCopySourceLinkError(err error) bool {
	return errors.Is(err, unix.ELOOP)
}

func isCopySourceUnsupportedOpenError(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENXIO)
}
