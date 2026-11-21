//go:build windows

package container

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCopyStagingDirectoryHasCurrentUserOnlyDACL(t *testing.T) {
	root, _, err := prepareCopyStagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	file, reparse, err := openCopySource(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if reparse {
		t.Fatal("staging directory opened as a reparse point")
	}
	if err := verifyCurrentUserOnlyDACL(file); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCopyStagingRejectsForeignDirectoryOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "preexisting")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Skipf("foreign SID unavailable: %v", err)
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		foreignSID,
		nil,
		nil,
		nil,
	); err != nil {
		t.Skipf("cannot create foreign-owned test directory: %v", err)
	}
	t.Cleanup(func() {
		_ = windows.SetNamedSecurityInfo(
			dir,
			windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION,
			currentUser.User.Sid,
			nil,
			nil,
			nil,
		)
	})

	if err := ensureWindowsPrivateDir(dir, false); err == nil {
		t.Fatal("foreign-owned staging directory was accepted")
	}
}

func TestWindowsStagingAncestryUsesCaseInsensitiveHandleIdentity(t *testing.T) {
	source := filepath.Join(t.TempDir(), "CaseSensitiveTree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := openVerifiedCopySource(source, openCopySource)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.file.Close()

	alias := filepath.Join(filepath.Dir(source), strings.ToUpper(filepath.Base(source)))
	if err := ensureCopyStagingOutsideSource(alias, opened); err == nil {
		t.Fatal("case-insensitive source alias bypassed staging ancestry check")
	}
}

func TestWindowsCopySourceRejectsReparsePoint(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Windows symlink unavailable: %v", err)
	}

	_, err := openVerifiedCopySource(link, openCopySource)
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
}

func TestWindowsCopyFileOpenRejectsReparsePoint(t *testing.T) {
	skipIfCopyFileOpenUnsupported(t)
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Windows symlink unavailable: %v", err)
	}

	file, err := openCopyFile(link)
	if file != nil {
		_ = file.Close()
		t.Fatal("openCopyFile returned a reparse-point handle")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Fatalf("error = %v, want ErrCopyFileNotRegular", err)
	}
}

func TestWindowsCopyStagingDACLRequiresCurrentUserAllowACE(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	tests := []struct {
		name    string
		sddl    string // empty selects a NULL DACL
		wantErr bool
		wantMsg string
	}{
		{name: "current user only", sddl: "D:P(A;;GA;;;" + sid + ")"},
		{name: "current user and Administrators", sddl: "D:P(A;;GA;;;" + sid + ")(A;;GA;;;BA)"},
		{name: "current user and SYSTEM", sddl: "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"},
		{name: "empty", sddl: "D:P", wantErr: true},
		{name: "deny only", sddl: "D:P(D;;GA;;;" + sid + ")", wantErr: true},
		{name: "Administrators only", sddl: "D:P(A;;GA;;;BA)", wantErr: true, wantMsg: "no current-user allow ACE"},
		{name: "SYSTEM only", sddl: "D:P(A;;GA;;;SY)", wantErr: true, wantMsg: "no current-user allow ACE"},
		{name: "Administrators and SYSTEM", sddl: "D:P(A;;GA;;;BA)(A;;GA;;;SY)", wantErr: true, wantMsg: "no current-user allow ACE"},
		// A NULL DACL grants everyone full access.
		{name: "NULL", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "staging")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Install the DACL through the handle that will also verify it.
			// Reopening the path under a restrictive DACL can fail with
			// access denied before verification even runs.
			file := openDirectoryForSecurityWrite(t, dir)
			t.Cleanup(func() {
				defer func() {
					if err := file.Close(); err != nil {
						t.Errorf("close staging handle: %v", err)
					}
				}()
				// Restore access before t.TempDir removes the directory.
				setProtectedTestDACL(t, file, "D:P(A;;GA;;;"+sid+")")
			})

			setProtectedTestDACL(t, file, tt.sddl)
			if _, err := windows.GetSecurityInfo(
				windows.Handle(file.Fd()),
				windows.SE_FILE_OBJECT,
				windows.OWNER_SECURITY_INFORMATION,
			); err != nil {
				t.Fatalf("staging handle is invalid after setting DACL: %v", err)
			}
			verifyErr := verifyCurrentUserOnlyDACL(file)
			if tt.wantErr && verifyErr == nil {
				t.Fatal("DACL was accepted")
			}
			if tt.wantErr && tt.wantMsg != "" && !strings.Contains(verifyErr.Error(), tt.wantMsg) {
				t.Fatalf("DACL error = %v, want %q", verifyErr, tt.wantMsg)
			}
			if !tt.wantErr && verifyErr != nil {
				t.Fatalf("DACL was rejected: %v", verifyErr)
			}
		})
	}
}

// setProtectedTestDACL protects the DACL so a rejection cannot come from the
// inheritance check instead of the ACE contents. It writes through an already
// open handle so installing a restrictive DACL cannot revoke the access
// needed to read it back.
func setProtectedTestDACL(t *testing.T, file *os.File, sddl string) {
	t.Helper()
	var acl *windows.ACL
	if sddl != "" {
		descriptor, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err = descriptor.DACL()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		t.Fatalf("set DACL %q: %v", sddl, err)
	}
}

// openDirectoryForSecurityWrite keeps WRITE_DAC and READ_CONTROL on the
// handle for the whole subtest. The access check runs once here, before the
// restrictive DACL is installed, so later checks and cleanup never need to
// reopen the path.
func openDirectoryForSecurityWrite(t *testing.T, dir string) *os.File {
	t.Helper()
	pathPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.READ_CONTROL|windows.WRITE_DAC,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatalf("open %s for READ_CONTROL|WRITE_DAC: %v", dir, err)
	}
	return os.NewFile(uintptr(handle), dir)
}

func TestWindowsFallbackCopyStagingRootIsRemoved(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "containergo"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, rootCleanup, err := prepareWindowsCopyStagingRootForSource(base, nil)
	if err != nil {
		t.Fatalf("prepare fallback staging root: %v", err)
	}
	appRoot := filepath.Dir(root)
	if !strings.HasPrefix(filepath.Base(appRoot), "containergo-") {
		t.Fatalf("staging root %q does not use a fallback parent", root)
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupCopyStagingDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := rootCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(appRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fallback staging parent %q remains after cleanup: %v", appRoot, err)
	}
}
