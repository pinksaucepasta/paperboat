//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// Users may create folders under ProgramData, so C:\ProgramData\Paperboat can be planted before install.
func TestExecutableDirectoryOwnedByAnotherAccountIsRefusedUnchanged(t *testing.T) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !token.IsElevated() || user.User.Sid.String() == "S-1-5-18" {
		t.Skip("elevated enrolled administrator fixture required")
	}
	path := filepath.Join(t.TempDir(), "Paperboat")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	const planted = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;BU)"
	descriptor, err := windows.SecurityDescriptorFromString(planted)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, dacl, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsExecutableDirectory(path, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ensureWindowsExecutableDirectory = %v, want a refusal", err)
	}
	if !windowssecurity.OwnerMatchesSID(path, user.User.Sid) || !windowssecurity.ProtectedDACLMatches(path, planted) {
		t.Fatal("refused directory was changed")
	}
	// The same directory made by an administrator is re-secured as before.
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, administrators, nil, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsExecutableDirectory(path, ""); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(path, system) || !windowssecurity.ProtectedDACLMatches(path, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)") {
		t.Fatal("administrator-made directory was not re-secured")
	}
}

// A refused pin cleanup reaches the reconciliation fault with its own stage, not a bare invalid request.
func TestPinCleanupFailureReportsItsStage(t *testing.T) {
	err := pinCleanupFailure{cause: hostInstallFailure("Windows runtime owner or DACL check failed at pin declaration directory", ErrInvalidRequest)}
	fault := errorreport.ProjectFault(context.Background(), "paperboatd", "service", "reconciliation", "service_failed", err)
	if fault.Stage != "pin_cleanup" || !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "pin declaration directory") {
		t.Fatalf("stage %q, error %q", fault.Stage, err)
	}
}
