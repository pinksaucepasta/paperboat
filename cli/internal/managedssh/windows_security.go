//go:build windows

package managedssh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

const windowsSystemSID = "S-1-5-18"

func currentManagedSSHSID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", ErrAgentDenied
	}
	return user.User.Sid.String(), nil
}

func managedSSHSDDL(sid string) string {
	return "O:" + sid + "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")"
}

func managedSSHPipeSDDL(sid string) string {
	return "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"
}

// withManagedSSHOwner keeps same-directory staging files from inheriting an
// elevated token's Administrators default owner. A normal user token already
// has the enrolled user as its default owner and needs no privilege change.
func withManagedSSHOwner(sid string, operation func() error) error {
	owner, err := windows.StringToSid(sid)
	if err != nil || owner == nil || !owner.IsValid() || operation == nil {
		return ErrOpenSSHConfigConflict
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	effectiveOwner, err := currentEffectiveTokenOwnerSID()
	if err != nil {
		return err
	}
	if effectiveOwner == sid {
		return operation()
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil || effectiveOwner != administrators.String() {
		return ErrOpenSSHConfigConflict
	}
	return windowssecurity.WithRestorePrivilegeAndOwner(owner, operation)
}

func currentEffectiveTokenOwnerSID() (string, error) {
	var token windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token)
	if errors.Is(err, windows.ERROR_NO_TOKEN) {
		err = windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	}
	if err != nil {
		return "", err
	}
	defer token.Close()
	var size uint32
	err = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size < uint32(unsafe.Sizeof(uintptr(0))) {
		if err == nil {
			err = windows.ERROR_INVALID_OWNER
		}
		return "", err
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], uint32(len(buffer)), &size); err != nil {
		return "", err
	}
	owner := (*struct{ Owner *windows.SID })(unsafe.Pointer(&buffer[0])).Owner
	if owner == nil || !owner.IsValid() {
		return "", ErrOpenSSHConfigConflict
	}
	value := owner.String()
	runtime.KeepAlive(buffer)
	return value, nil
}

func rejectWindowsReparseAncestors(path string) error {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	if volume == "" || !filepath.IsAbs(path) {
		return errors.New("managed SSH path must be an absolute Windows path")
	}
	root := volume + string(filepath.Separator)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("managed SSH path escapes its volume")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || windowsReparsePoint(current) {
			return errors.New("managed SSH path contains a reparse point")
		}
	}
	return nil
}

func windowsReparsePoint(path string) bool {
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	return err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func verifyManagedSSHACL(path, sid string) error {
	owner, err := windows.StringToSid(sid)
	if err != nil || owner == nil || !windowssecurity.OwnerMatchesSID(path, owner) || !windowssecurity.ProtectedDACLMatches(path, managedSSHSDDL(sid)) {
		return ErrAuthorizedKeysConflict
	}
	return nil
}

func applyManagedSSHACL(path, sid string) error {
	descriptor, err := windows.SecurityDescriptorFromString(managedSSHSDDL(sid))
	if err != nil {
		return err
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return err
	}
	owner, _, err := absolute.Owner()
	if err != nil || owner == nil {
		return ErrAuthorizedKeysConflict
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
}

func ensureManagedSSHDirectory(path, sid string) error {
	if err := rejectWindowsReparseAncestors(filepath.Dir(path)); err != nil {
		return err
	}
	if err := withManagedSSHOwner(sid, func() error { return os.MkdirAll(path, 0o700) }); err != nil {
		return err
	}
	if err := rejectWindowsReparseAncestors(path); err != nil {
		return err
	}
	if err := applyManagedSSHACL(path, sid); err != nil {
		return fmt.Errorf("protect managed SSH directory: %w", err)
	}
	return verifyManagedSSHACL(path, sid)
}

// verifyCurrentUserOwnedPath applies Win32-OpenSSH's own rule for ~/.ssh (check_secure_file_permission in
// PowerShell/openssh-portable contrib/win32/win32compat/w32-sshfileperm.c): the owner is the user, SYSTEM,
// Administrators or TrustedInstaller, and no other principal holds a write-class right. An elevated shell makes
// Administrators the owner of a folder it creates, so the user's own ~/.ssh is often owned that way.
func verifyCurrentUserOwnedPath(path, sid string, requireDirectory bool) error {
	if err := rejectWindowsReparseAncestors(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || (requireDirectory && !info.IsDir()) || (!requireDirectory && !info.Mode().IsRegular()) || windowsReparsePoint(path) {
		return ErrOpenSSHConfigConflict
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	return sshDescriptorError(descriptor, err, path, sid)
}

// verifySecureSSHHandle applies the same Win32-OpenSSH rule to an open file the user owns the content of
// (~/.ssh/config, authorized_keys); path names it in the refusal.
func verifySecureSSHHandle(handle windows.Handle, path, sid string) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	return sshDescriptorError(descriptor, err, path, sid)
}

func sshDescriptorError(descriptor *windows.SECURITY_DESCRIPTOR, err error, path, sid string) error {
	if err != nil || descriptor == nil {
		return ErrOpenSSHConfigConflict
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return ErrOpenSSHConfigConflict
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return ErrOpenSSHConfigConflict
	}
	if problem := sshPathSecurityProblem(owner, dacl, sid); problem != "" {
		return &UnsafeSSHPathError{Path: path, Problem: problem}
	}
	return nil
}

const (
	windowsAdministratorsSID  = "S-1-5-32-544"
	windowsTrustedInstallerID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	// Win32-OpenSSH's SSH_SECURE_WRITE_MASK, plus FILE_DELETE_CHILD (another principal could delete files in
	// the folder) and the generic write rights an inheritable rule may still hold unmapped.
	windowsFileDeleteChild = 0x40 // FILE_DELETE_CHILD, not defined in x/sys/windows
	sshPathWriteMask       = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windowsFileDeleteChild |
		windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
)

func trustedSSHPathPrincipal(sid *windows.SID, userSID string) bool {
	switch sid.String() {
	case userSID, windowsSystemSID, windowsAdministratorsSID, windowsTrustedInstallerID:
		return true
	}
	return false
}

// sshPathSecurityProblem says in words why OpenSSH must not trust a path with this owner and DACL, or "" when it may.
func sshPathSecurityProblem(owner *windows.SID, dacl *windows.ACL, userSID string) string {
	if !trustedSSHPathPrincipal(owner, userSID) {
		return "it is owned by " + windowsAccountName(owner) + ", not by you, SYSTEM or Administrators"
	}
	if dacl == nil {
		return "it has no access list, so every user can change it"
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, index, &ace) != nil || ace == nil {
			return "its access list cannot be read"
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			// Object and conditional rules are not plain user rights; refuse rather than guess what they grant.
			return "it has an access rule Paperboat cannot check"
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask&sshPathWriteMask != 0 && !trustedSSHPathPrincipal(trustee, userSID) {
			return windowsAccountName(trustee) + " can change it"
		}
	}
	return ""
}

func windowsAccountName(sid *windows.SID) string {
	if account, domain, _, err := sid.LookupAccount(""); err == nil && account != "" {
		if domain != "" {
			return domain + `\` + account
		}
		return account
	}
	return sid.String()
}

// verifyCurrentUserProfileRoot accepts SYSTEM ownership only for the existing
// Windows profile root. Provisioning and profile migration commonly leave
// C:\Users\<name> owned by SYSTEM even though the enrolled user owns .ssh.
// Writable descendants continue through verifyCurrentUserOwnedPath and must be
// owned by the exact current-user SID.
func verifyCurrentUserProfileRoot(path, sid string) error {
	if err := rejectWindowsReparseAncestors(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrOpenSSHConfigConflict
	}
	reparse := windowsReparsePoint(path)
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return ErrOpenSSHConfigConflict
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !validWindowsProfileRootState(info.Mode(), reparse, owner.String(), sid) {
		return ErrOpenSSHConfigConflict
	}
	return nil
}

func validWindowsProfileRootState(mode os.FileMode, reparse bool, owner, sid string) bool {
	return mode.IsDir() && mode&os.ModeSymlink == 0 && !reparse && (owner == sid || owner == windowsSystemSID)
}
