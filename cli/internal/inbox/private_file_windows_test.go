//go:build windows

package inbox

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// requirePrivateFile fails unless only the current user and SYSTEM can reach the delivered file.
// Windows reports mode 0666 for every file, so the ACL is the only thing to check.
func requirePrivateFile(t *testing.T, path string) {
	t.Helper()
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		t.Fatalf("%s has no DACL (everyone has access): %v", path, err)
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user) && !sid.Equals(system) {
			t.Fatalf("%s grants access to %s", path, sid)
		}
	}
}
