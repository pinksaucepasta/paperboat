//go:build windows

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestSharedProgramRootDescriptor(t *testing.T) {
	for _, tt := range []struct {
		name, sddl string
		valid      bool
	}{
		{"canonical", "O:SY" + sharedProgramRootDACL, true},
		{"program files bootstrap", "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;OICIIO;GA;;;CO)(A;OICIIO;GXGR;;;BU)(A;;0x1200a9;;;BU)(A;;FA;;;" + trustedInstallerSID + ")", true},
		{"generic read execute", "O:BAD:P(A;;FA;;;BA)(A;;GRGX;;;BU)", true},
		{"effective user write", "O:BAD:P(A;;FA;;;BA)(A;;GW;;;BU)", false},
		{"unknown writer", "O:BAD:P(A;;FA;;;BA)(A;;FA;;;S-1-5-21-1-2-3-1001)", false},
		{"user owner", "O:S-1-5-21-1-2-3-1001" + sharedProgramRootDACL, false},
		{"unprotected", "O:BAD:(A;;FA;;;BA)", false},
		{"null dacl", "O:BAD:PNO_ACCESS_CONTROL", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tt.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if got := safeSharedProgramRootDescriptor(sd); got != tt.valid {
				t.Fatalf("accepted=%t want=%t", got, tt.valid)
			}
		})
	}
}

func TestNativeSharedProgramRootMigrationPreservesChild(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated Windows filesystem qualification")
	}
	root := filepath.Join(t.TempDir(), "Paperboat")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	beforeDACL := "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICIIO;GA;;;CO)(A;;GRGX;;;BU)(A;;FA;;;" + trustedInstallerSID + ")"
	// Windows maps generic file rights while applying this migration input.
	// Establish it through the native API rather than the production helper's
	// deliberately exact canonical-descriptor comparison.
	descriptor, err := windows.SecurityDescriptorFromString(beforeDACL)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			admins, nil, dacl, nil)
	}); err != nil {
		t.Fatal(err)
	}
	established, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	establishedOwner, _, err := established.Owner()
	if err != nil || establishedOwner == nil || !establishedOwner.Equals(admins) || !safeSharedProgramRootDescriptor(established) {
		t.Fatal("native migration fixture did not establish trusted protected input")
	}
	child := filepath.Join(root, "owner-state")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureSharedProgramRoot(root); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if !windowsRuntimeSecurityMatches(root, system, sharedProgramRootDACL) {
		t.Fatal("shared root is not canonical")
	}
	after, err := windows.GetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("child security descriptor changed")
	}
	link := filepath.Join(filepath.Dir(root), "redirected")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := secureSharedProgramRoot(link); err == nil {
		t.Fatal("accepted reparse directory")
	}
}
