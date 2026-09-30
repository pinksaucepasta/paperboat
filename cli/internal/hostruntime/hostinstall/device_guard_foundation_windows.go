//go:build windows

package hostinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const sharedProgramRootDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// The privileged installer owns the shared product foundation. Child objects
// retain their separate owners and ACLs; this operation never walks the tree.
func prepareDeviceGuardFoundation() error {
	parent, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return err
	}
	if !safeAbsolute(parent) || !strings.EqualFold(filepath.Clean(os.Getenv("ProgramFiles")), parent) {
		return ErrInvalidRequest
	}
	if err := rejectWindowsReparseAncestors(parent); err != nil {
		return err
	}
	return secureSharedProgramRoot(filepath.Join(parent, "Paperboat"))
}

func secureSharedProgramRoot(path string) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(path); os.IsNotExist(err) {
		return createWindowsRuntimeRoot(path, system, sharedProgramRootDACL)
	} else if err != nil {
		return err
	}
	// Exclusive sharing prevents SetSecurityInfo from propagating ACL changes
	// to existing children (the documented directory-handle behavior).
	handle, _, err := openWindowsRuntimeObject(path, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if !safeSharedProgramRootDescriptor(descriptor) {
		return fmt.Errorf("existing Paperboat shared program directory permits untrusted ownership or access: %w", ErrInvalidRequest)
	}
	return applyWindowsHandleOwnedDACL(handle, system, sharedProgramRootDACL)
}

func safeSharedProgramRootDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) bool {
	if descriptor == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	control, _, controlErr := descriptor.Control()
	if err != nil || controlErr != nil || owner == nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	if owner.String() != "S-1-5-18" && owner.String() != "S-1-5-32-544" {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return false
		}
		// An inherit-only ACE grants nothing on this directory. Newly created
		// children will receive the canonical ACL after this foundation is secured.
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch sid.String() {
		case "S-1-5-18", "S-1-5-32-544", trustedInstallerSID:
			continue
		}
		mask := uint32(ace.Mask)
		// File generic mappings, including directory traversal/list semantics.
		for _, mapping := range []struct{ generic, specific uint32 }{
			{windows.GENERIC_READ, windows.FILE_GENERIC_READ},
			{windows.GENERIC_EXECUTE, windows.FILE_GENERIC_EXECUTE},
			{windows.GENERIC_WRITE, windows.FILE_GENERIC_WRITE},
			{windows.GENERIC_ALL, 0x001f01ff},
		} {
			if mask&mapping.generic != 0 {
				mask = mask&^mapping.generic | mapping.specific
			}
		}
		if mask&^uint32(windows.FILE_GENERIC_READ|windows.FILE_GENERIC_EXECUTE) != 0 {
			return false
		}
	}
	return true
}
