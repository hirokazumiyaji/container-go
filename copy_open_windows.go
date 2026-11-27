//go:build windows

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsCopyFileOpenFlags are the CreateFile flags os.OpenFile must pass
// through on Windows. Go 1.26 and later (the minimum this module supports)
// honor them; older toolchains silently dropped them, which is why the
// capability check used to be conditional.
const windowsCopyFileOpenFlags = syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.FILE_FLAG_OVERLAPPED

// checkCopyFileOpenCapability reports whether this host can open a copied
// file without following links or blocking on special files. Windows builds
// require Go 1.26+, so the capability is always present here.
func checkCopyFileOpenCapability() error { return nil }

// openCopyFile opens the reparse point itself and uses overlapped I/O
// so a named-pipe target cannot turn the read into an unbounded wait.
// FILE_FLAG_OPEN_REPARSE_POINT reports the link itself instead of its
// target, so the handle must be rejected before any caller reads it.
func openCopyFile(name string) (*os.File, error) {
	if err := checkCopyFileOpenCapability(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(name, os.O_RDONLY|windowsCopyFileOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	reparse, err := windowsCopyHandleIsReparse(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if reparse {
		_ = file.Close()
		return nil, fmt.Errorf("opened a reparse point: %w", ErrCopyFileNotRegular)
	}
	return file, nil
}

// lstatCopyFile reads metadata and the file ID from one no-follow handle.
// os.Lstat defers the ID until os.SameFile reopens the path, which would
// compare the opened handle with whatever the path names at that later time.
func lstatCopyFile(name string) (os.FileInfo, error) {
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(name))
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: name, Err: err}
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_READ_ATTRIBUTES,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(handle), name)
	defer func() { _ = file.Close() }()
	return file.Stat()
}

const copySourceShareMode = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

func copySourceFileIdentity(file *os.File) (copySourceIdentity, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return copySourceIdentity{}, err
	}
	return copySourceIdentity{
		primary:   uint64(info.VolumeSerialNumber),
		secondary: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func sameCopySourceIdentity(first, second copySourceIdentity) bool {
	return first.primary == second.primary && first.secondary == second.secondary
}

// openCopySource opens the final component without following a reparse point.
// Sharing delete lets a replacement race occur while the snapshot still reads
// only the handle it opened and validated.
func openCopySource(path string) (*os.File, bool, error) {
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_GENERIC_READ,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(handle), path)
	reparse, err := windowsCopyHandleIsReparse(file)
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	return file, reparse, nil
}

// openCopySourceAt uses an NT relative open against the parent directory
// handle. FILE_OPEN_REPARSE_POINT applies before the handle is validated, so
// neither a child replacement nor a child reparse point can redirect reads.
func openCopySourceAt(parent *os.File, name string) (*os.File, bool, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, false, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ,
		&attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		copySourceShareMode,
		windows.FILE_OPEN,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return nil, false, mapWindowsCopyOpenError(err)
	}
	file := os.NewFile(uintptr(handle), filepath.Join(parent.Name(), name))
	reparse, err := windowsCopyHandleIsReparse(file)
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	return file, reparse, nil
}

func windowsCopyHandleIsReparse(file *os.File) (bool, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return false, err
	}
	return info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func mapWindowsCopyOpenError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

func isCopyStagingRootParentError(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_NAME) || errors.Is(err, windows.ERROR_BAD_PATHNAME)
}

func windowsExtendedPath(path string) string {
	if len(path) >= 4 && (strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`)) {
		return path
	}
	if strings.HasPrefix(path, `\\.\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
	}
	return `\\?\` + path
}

func isCopySourceLinkError(error) bool { return false }

func isCopySourceUnsupportedOpenError(error) bool { return false }
