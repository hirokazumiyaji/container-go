//go:build windows

package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCopyStagingDirectoryHasCurrentUserOnlyDACL(t *testing.T) {
	root, err := prepareCopyStagingRoot()
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

	if err := ensureWindowsPrivateCopyStagingDir(dir, false); err == nil {
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
