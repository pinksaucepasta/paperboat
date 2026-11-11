//go:build windows

package windowsopenssh

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// The owner SID goes into the config's SDDL, so only a SID Windows parses back
// to the same text may: nothing that could close the ACE and add another.
func validOwnerSID(owner string) bool {
	sid, err := windows.StringToSid(owner)
	return err == nil && sid.String() == owner
}

// The managed sshd_config: SYSTEM and Administrators full, the owner reads it,
// nothing inherited from the folder.
func serviceConfigFileOptions(owner string) atomicfile.Options {
	return atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + generationFileDACL(owner)}
}

// Paperboat's key file: SYSTEM and Administrators only, the rule Win32-OpenSSH
// sets for administrators_authorized_keys, nothing inherited. The owner never
// reads it: keys arrive through the host service's SID-restricted pipe.
func managedKeysFileOptions() atomicfile.Options {
	return atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: managedKeysSecurity}
}

const managedKeysSecurity = "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)"

func setManagedKeysSecurity(path string) error {
	descriptor, err := windows.SecurityDescriptorFromString(managedKeysSecurity)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
	})
}

func platformOwnerSID() string {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return ""
	}
	return user.User.Sid.String()
}

// Account lookup uses the local SAM's NetBIOS name. os.Hostname reports the
// physical DNS name, which can differ and must not turn a local user into a
// domain-qualified Match pattern.
func platformSSHComputerName() (string, error) {
	var name [16]uint16 // NetBIOS computer names have at most 15 characters.
	size := uint32(len(name))
	if err := windows.GetComputerNameEx(windows.ComputerNameNetBIOS, &name[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(name[:size]), nil
}
