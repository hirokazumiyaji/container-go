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
	return prepareCopyStagingRootForSource(nil)
}

func prepareCopyStagingRootForSource(source *openedCopySource) (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("copy staging: locate per-user LocalAppData: %w", err)
	}
	if err := ensureCopyStagingOutsideSource(base, source); err != nil {
		return "", err
	}
	appRoot := filepath.Join(base, "containergo")
	if err := ensureWindowsPrivateCopyStagingDir(appRoot, true); err != nil {
		// Shared CI images can already have LocalAppData\containergo
		// owned by another logon SID. Create a unique private parent
		// rather than writing into that directory.
		var random [8]byte
		if _, randErr := rand.Read(random[:]); randErr != nil {
			return "", fmt.Errorf("copy staging: create private staging parent: %w", err)
		}
		appRoot = filepath.Join(base, "containergo-"+hex.EncodeToString(random[:]))
		if retryErr := ensureWindowsPrivateCopyStagingDir(appRoot, true); retryErr != nil {
			return "", fmt.Errorf("copy staging: create private staging parent: %w", retryErr)
		}
	}
	root := filepath.Join(appRoot, "copy-staging")
	if err := ensureWindowsPrivateCopyStagingDir(root, true); err != nil {
		return "", fmt.Errorf("copy staging: create private staging root: %w", err)
	}
	return root, nil
}

func ensureWindowsPrivateCopyStagingDir(path string, create bool) error {
	return configureWindowsPrivateCopyStagingDir(path, create, false)
}

func configureWindowsPrivateCopyStagingDir(path string, create, created bool) error {
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
		switch {
		case err == nil:
			created = true
		case !errors.Is(err, windows.ERROR_ALREADY_EXISTS):
			return err
		}
	}

	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return err
	}
	access := uint32(windows.GENERIC_READ | windows.WRITE_DAC | windows.READ_CONTROL)
	if created {
		access |= windows.WRITE_OWNER
	}
	handle, err := windows.CreateFile(
		pathPtr,
		access,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil && created {
		handle, err = windows.CreateFile(
			pathPtr,
			windows.GENERIC_READ|windows.WRITE_DAC|windows.READ_CONTROL,
			copySourceShareMode,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
			0,
		)
	}
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
	// GitHub-hosted Windows runners default new directory owners to
	// BUILTIN\Administrators, which is not the token user SID. A directory
	// we just created is still ours; keep the owner check for a pre-existing
	// path that another logon may have planted.
	if !created {
		if err := verifyCurrentUserOwner(file); err != nil {
			return err
		}
	} else {
		_ = setCurrentUserOwner(file)
	}
	if err := setCurrentUserOnlyDACL(file); err != nil {
		return err
	}
	if err := verifyCurrentUserOnlyDACL(file); err != nil {
		// A first SetSecurityInfo can convert inherited ACEs to explicit
		// ones without dropping them. Retry once against the now-protected
		// descriptor.
		if retryErr := setCurrentUserOnlyDACL(file); retryErr != nil {
			return err
		}
		if err := verifyCurrentUserOnlyDACL(file); err != nil {
			return err
		}
	}
	if !created {
		if err := verifyCurrentUserOwner(file); err != nil {
			return err
		}
	}
	return nil
}

func verifyCurrentUserOwner(file *os.File) error {
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !windows.EqualSid(owner, user.User.Sid) {
		return fmt.Errorf("staging directory is owned by a foreign SID")
	}
	return nil
}

func setCurrentUserOwner(file *os.File) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		user.User.Sid,
		nil,
		nil,
		nil,
	)
}

func setCurrentUserOnlyDACL(file *os.File) error {
	acl, err := currentUserOnlyACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func currentUserOnlyACL() (*windows.ACL, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
}

func allowedStagingSIDs() ([]*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	sids := []*windows.SID{user.User.Sid}
	if admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err == nil {
		sids = append(sids, admin)
	}
	if system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid); err == nil {
		sids = append(sids, system)
	}
	return sids, nil
}

func sidAllowedForStaging(sid *windows.SID, allowed []*windows.SID) bool {
	for _, candidate := range allowed {
		if windows.EqualSid(sid, candidate) {
			return true
		}
	}
	return false
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
	// DACL() reports (acl, defaulted, err). An explicit SetSecurityInfo
	// DACL is not defaulted; the old !present check treated that as empty.
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	aceCount := 0
	if dacl != nil {
		aceCount = int(dacl.AceCount)
	}
	if dacl == nil || aceCount < 1 {
		return fmt.Errorf("staging DACL has %d ACEs, want a current-logon ACE", aceCount)
	}
	allowed, err := allowedStagingSIDs()
	if err != nil {
		return err
	}
	for i := 0; i < aceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("staging DACL ACE type is %d, want allow", ace.Header.AceType)
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sidAllowedForStaging(aceSID, allowed) {
			return fmt.Errorf("staging DACL contains a non-current-user ACE")
		}
	}
	return nil
}

func currentUserSecurityAttributes() (*windows.SecurityAttributes, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return nil, err
	}
	if err := absolute.SetOwner(user.User.Sid, false); err != nil {
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
		if err := configureWindowsPrivateCopyStagingDir(path, false, true); err != nil {
			_ = os.RemoveAll(path)
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("copy staging: could not allocate a private staging directory")
}
