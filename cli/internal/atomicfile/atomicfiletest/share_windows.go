//go:build windows

package atomicfiletest

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"golang.org/x/sys/windows"
)

// MakeAccessible undoes WriteOwnerOnly: the file goes back to inheriting its directory's ACL, which is how a
// careless user or tool leaves it (a protected owner-only DACL is what the product demands).
func MakeAccessible(t testing.TB, path string) {
	t.Helper()
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// WriteOwnerAndSystemOnly is WriteOwnerOnly for the files whose Windows form allows exactly SYSTEM and the owner
// (no Administrators entry), such as enrollment token files; on Unix both are mode 0600.
func WriteOwnerAndSystemOnly(t testing.TB, path string, data []byte) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := "D:P(A;;FA;;;SY)"
	if user.User.Sid.String() != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + user.User.Sid.String() + ")"
	}
	if err := atomicfile.Write(path, data, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: descriptor}); err != nil {
		t.Fatalf("write owner-and-system-only fixture: %v", err)
	}
}
