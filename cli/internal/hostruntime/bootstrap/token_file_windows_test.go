//go:build windows

package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEnrollmentTokenFileWindowsProtectedACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment-token.txt")
	const token = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A plain new file inherits its folder's permissions: the refusal must say so, not call it "not a regular file".
	if _, err := ReadEnrollmentTokenFile(path); err == nil || !strings.Contains(err.Error(), "icacls") || !errors.Is(err, ErrInvalid) {
		t.Fatalf("plain file with inherited permissions: %v", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := "D:P(A;;FA;;;SY)"
	if user.User.Sid.String() != "S-1-5-18" {
		sddl += "(A;;FA;;;" + user.User.Sid.String() + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := sd.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := abs.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnrollmentTokenFile(path)
	if err != nil || got != token {
		t.Fatalf("ReadEnrollmentTokenFile() = %q, %v", got, err)
	}
	for _, test := range []struct {
		name, dacl string
		accepted   bool
	}{
		{"reordered allow entries", "D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)", user.User.Sid.String() != "S-1-5-18"},
		{"extra reader", sddl + "(A;;FR;;;WD)", false},
		{"partial user rights", "D:P(A;;FR;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.dacl)
			if err != nil {
				t.Fatal(err)
			}
			absolute, err := sd.ToAbsolute()
			if err != nil {
				t.Fatal(err)
			}
			acl, _, err := absolute.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			_, err = ReadEnrollmentTokenFile(path)
			if (err == nil) != test.accepted {
				t.Fatalf("accepted=%t, want %t", err == nil, test.accepted)
			}
		})
	}
	if err := ConsumeEnrollmentTokenFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file still exists: %v", err)
	}
}
