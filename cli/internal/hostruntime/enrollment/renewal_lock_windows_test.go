//go:build windows

package enrollment

import (
	"errors"
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
