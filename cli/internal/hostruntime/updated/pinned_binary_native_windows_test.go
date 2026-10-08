//go:build windows

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// Real PE bytes and OS ACLs exercise the immutable executable verification
// boundary. This test never changes an installed pin or touches SCM services.
func TestNativeWindowsPinnedBinaryPolicies(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.User.Sid.String() != "S-1-5-18" {
		t.Skip("requires native SYSTEM protected-file fixture")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pb.exe")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	target := workerupdate.ComponentTarget{SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body)), Platform: "windows", Architecture: runtime.GOARCH}
	ownerSID := "S-1-5-21-101-202-303-1001"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name, dacl string
		allowed    bool
		alter      string
	}{
		{"release_users_read_execute", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", true, ""},
		{"installer_enrolled_read_execute", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + ownerSID + ")", true, ""},
		{"users_write", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;BU)", false, ""},
		{"enrolled_write", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + ownerSID + ")", false, ""},
		{"unexpected_read_ace", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)(A;;FR;;;WD)", false, ""},
		{"wrong_owner", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", false, "owner"},
		{"unprotected", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", false, "unprotected"},
		{"wrong_digest", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", false, "digest"},
		{"wrong_length", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", false, "length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyWindowsReleaseACL(path, tc.dacl); err != nil {
				t.Fatal(err)
			}
			candidate := target
			switch tc.alter {
			case "owner":
				admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
				if err != nil {
					t.Fatal(err)
				}
				if err := windowssecurity.WithRestorePrivilege(func() error {
					return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, admin, nil, nil, nil)
				}); err != nil {
					t.Fatal(err)
				}
			case "unprotected":
				descriptor, err := windows.SecurityDescriptorFromString(tc.dacl)
				if err != nil {
					t.Fatal(err)
				}
				dacl, _, err := descriptor.DACL()
				if err != nil {
					t.Fatal(err)
				}
				if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
					t.Fatal(err)
				}
			case "digest":
				candidate.SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			case "length":
				candidate.Length++
			}
			// The exact former production gate rejects this valid release pin;
			// it remains correct for the separate canonical binary policy.
			if tc.name == "release_users_read_execute" {
				if err := verifyWindowsStableBinary(ctx, path, target, ownerSID); !errors.Is(err, errInvalidWindowsActivation) {
					t.Fatalf("former canonical gate must reject Users pin: %v", err)
				}
			}
			err := verifyWindowsPinnedBinary(ctx, path, candidate, ownerSID)
			if tc.allowed && err != nil {
				t.Fatalf("valid immutable pin rejected: %v", err)
			}
			if !tc.allowed && !errors.Is(err, errInvalidWindowsActivation) {
				t.Fatalf("invalid immutable pin accepted or wrong rejection: %v", err)
			}
		})
	}
}
