//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"golang.org/x/sys/windows"
)

// protectCredentialTestFile gives a test reference the protected DACL the daemon's reader requires (SYSTEM,
// Administrators and the current user, nothing inherited).
func protectCredentialTestFile(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// The prompt once skipped every check on Windows, so it saved references with an inherited (open) ACL that the
// daemon then refused at sync time.
func TestConfigCredentialInteractiveWindowsRejectsInheritedACL(t *testing.T) {
	path := filepath.Join(shorttemp.Dir(t), "known_hosts")
	if err := os.WriteFile(path, []byte("reference"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialInteractiveFile(path); err == nil {
		t.Fatal("reference with an inherited ACL accepted")
	}
	protectCredentialTestFile(t, path)
	if err := validateCredentialInteractiveFile(path); err != nil {
		t.Fatal(err)
	}
}
