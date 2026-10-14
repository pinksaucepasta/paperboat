//go:build windows

package configsync

import (
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"strings"
	"unsafe"
)

func privateRepositoryCredentialDirectory(path string, info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || !windowssecurity.OwnerMatchesSID(path, user.User.Sid) {
		return false
	}
	want, err := currentPrivateFileDescriptor()
	return err == nil && windowssecurity.ProtectedDACLMatches(path, want.String())
}

func protectRepositoryCredentialDirectory(path string, _ os.FileInfo) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return ErrRepositoryCredentials
	}
	want, err := currentPrivateFileDescriptor()
	if err != nil {
		return err
	}
	acl, _, err := want.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// References on Windows must have an explicitly protected owner-private ACL;
// POSIX permission bits do not represent Windows access rights.
func secureRepositoryReference(path string, info os.FileInfo, private bool) bool {
	return privateControlFile(path, info)
}

// Windows preserves original spelling while resolving filesystem casing. Accept
// a casing-only difference only after proving that both names identify the same
// filesystem object; junctions and path aliases still fail the lexical check.
func repositoryReferencePathMatches(path, resolved string) bool {
	if path == resolved {
		return true
	}
	if !strings.EqualFold(path, resolved) {
		return false
	}
	original, err := os.Stat(path)
	if err != nil {
		return false
	}
	actual, err := os.Stat(resolved)
	return err == nil && os.SameFile(original, actual)
}

// An elevated Windows token defaults a new directory's owner to Administrators.
// Set the current user owner and protected ACL in CreateDirectory itself; an
// existing directory still passes the strict owner guard before protection.
func createRepositoryCredentialDirectory(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := currentPrivateFileDescriptor()
	if err != nil {
		return err
	}
	ownerPrivate, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + descriptor.String())
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: ownerPrivate}
	err = windows.CreateDirectory(name, attrs)
	if err == windows.ERROR_ALREADY_EXISTS {
		return os.ErrExist
	}
	return err
}
