//go:build windows

package container

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareCopyStagingRoot() (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("copy to container: locate per-user LocalAppData: %w", err)
	}
	appRoot := filepath.Join(base, "containergo")
	if err := ensureWindowsPrivateCopyStagingDir(appRoot, true); err != nil {
		return "", fmt.Errorf("copy to container: create private staging parent: %w", err)
	}
	root := filepath.Join(appRoot, "copy-staging")
	if err := ensureWindowsPrivateCopyStagingDir(root, true); err != nil {
		return "", fmt.Errorf("copy to container: create private staging root: %w", err)
	}
	return root, nil
}

func ensureWindowsPrivateCopyStagingDir(path string, create bool) error {
	if create {
		securityAttributes, err := currentUserSecurityAttributes()
		if err != nil {
			return err
		}
		pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
		if err != nil {
			return err
		}
		err = windows.CreateDirectory(pathPtr, securityAttributes)
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
	}

	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.WRITE_DAC|windows.READ_CONTROL,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()

	reparse, err := windowsCopyHandleIsReparse(file)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", path)
	}
	if err := setCurrentUserOnlyDACL(file); err != nil {
		return err
	}
	return verifyCurrentUserOnlyDACL(file)
}

func setCurrentUserOnlyDACL(file *os.File) error {
	securityAttributes, err := currentUserSecurityAttributes()
	if err != nil {
		return err
	}
	dacl, _, err := securityAttributes.SecurityDescriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

func verifyCurrentUserOnlyDACL(file *os.File) error {
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("staging DACL is inheritable")
	}
	dacl, present, err := descriptor.DACL()
	if err != nil {
		return err
	}
	aceCount := 0
	if dacl != nil {
		aceCount = int(dacl.AceCount)
	}
	if !present || dacl == nil || aceCount != 1 {
		return fmt.Errorf("staging DACL has %d ACEs, want one current-user ACE", aceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return fmt.Errorf("staging DACL ACE type is %d, want allow", ace.Header.AceType)
	}
	requiredInheritance := uint8(windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE)
	if ace.Header.AceFlags&requiredInheritance != requiredInheritance {
		return fmt.Errorf("staging DACL ACE is not inheritable")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !windows.EqualSid(aceSID, user.User.Sid) {
		return fmt.Errorf("staging DACL contains a non-current-user ACE")
	}
	return nil
}

func currentUserSecurityAttributes() (*windows.SecurityAttributes, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICII;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: absolute,
	}, nil
}

func createCopyStagingDir(root string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		path := filepath.Join(root, "copy-to-"+hex.EncodeToString(random[:]))
		securityAttributes, err := currentUserSecurityAttributes()
		if err != nil {
			return "", err
		}
		pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
		if err != nil {
			return "", err
		}
		err = windows.CreateDirectory(pathPtr, securityAttributes)
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := ensureWindowsPrivateCopyStagingDir(path, false); err != nil {
			_ = os.RemoveAll(path)
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("copy to container: could not allocate a private staging directory")
}
