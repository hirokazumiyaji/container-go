//go:build windows

package container

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const copySourceShareMode = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

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
