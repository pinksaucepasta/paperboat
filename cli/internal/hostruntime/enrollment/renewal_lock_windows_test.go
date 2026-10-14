//go:build windows

package enrollment

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeIdentityRenewalLockRejectsUntrustedStateRoot(t *testing.T) {
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect test state root", err)
	}
	if err := makeEnrollmentStateRootWorldWritableForTest(root); err != nil {
		t.Fatal("make test state root untrusted", err)
	}
	if _, err := openRuntimeIdentityRenewalLock(root); !errors.Is(err, ErrInvalid) {
		t.Fatalf("untrusted state root lock error=%v", err)
	}
}

func TestRuntimeIdentityRenewalLockAcceptsReadOnlyInheritedPrincipal(t *testing.T) {
	root := t.TempDir()
	if err := makeEnrollmentStateRootWorldReadableForTest(root); err != nil {
		t.Fatal("make test state root safely readable", err)
	}
	lock, err := openRuntimeIdentityRenewalLock(root)
	if err != nil {
		t.Fatalf("read-only access for unrelated principal rejected: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal("close renewal lock", err)
	}
}

func TestRuntimeIdentityRenewalLockCreatesExactEnrolledOwner(t *testing.T) {
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	lock, err := openRuntimeIdentityRenewalLock(root)
	if err != nil {
		t.Fatal("create account renewal lock:", err)
	}
	if !windowssecurity.HandleOwnerMatchesSID(windows.Handle(lock.file.Fd()), user.User.Sid) {
		lock.Close()
		t.Fatal("renewal lock inherited a different administrative owner")
	}
	acquired, err := lock.Try()
	if err != nil || !acquired {
		lock.Close()
		t.Fatal("acquire renewal lock:", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, runtimeIdentityRenewalLockFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, admins, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	lock, err = openRuntimeIdentityRenewalLock(root)
	if err != nil {
		t.Fatal("recover previous administrative owner:", err)
	}
	defer lock.Close()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !windowssecurity.HandleOwnerMatchesSID(windows.Handle(lock.file.Fd()), user.User.Sid) {
		t.Fatal("recovery replaced the shared lock or retained another owner")
	}
}
