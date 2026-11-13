//go:build windows

package configsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func setSnapshotOwner(t *testing.T, path, sid string) {
	t.Helper()
	owner, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	err = windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil)
	})
	if err != nil {
		t.Skipf("cannot set the owner to %s (needs an elevated token): %v", sid, err)
	}
}

func setSnapshotDACL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnsafeSnapshotReasonOwnersAndWriters(t *testing.T) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := user.User.Sid.String()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	check := func(label, want string) {
		t.Helper()
		if got := unsafeSnapshotReason(path, info); got != want {
			t.Fatalf("%s: reason %q, want %q", label, got, want)
		}
	}
	check("own file", "")
	setSnapshotOwner(t, path, "S-1-5-32-544")
	check("owned by Administrators (elevated editor)", "")
	setSnapshotOwner(t, path, "S-1-5-18")
	check("owned by SYSTEM", "")
	setSnapshotOwner(t, path, "S-1-5-32-546") // Guests: not the user, Administrators or SYSTEM
	check("owned by another ordinary account", "owned_by_other_user")
	setSnapshotOwner(t, path, me)
	setSnapshotDACL(t, path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+me+")(A;;FW;;;WD)")
	check("Everyone may write", "writable_by_other_users")
	setSnapshotDACL(t, path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+me+")(A;;FR;;;WD)")
	check("Everyone may only read", "")
}
