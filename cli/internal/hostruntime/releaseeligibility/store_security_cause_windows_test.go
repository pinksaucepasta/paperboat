//go:build windows

package releaseeligibility

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func nativeEligibilityFixture(t *testing.T) FileStore {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "protected-eligibility")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal("owned directory creation failed")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal("fixture system SID unavailable")
	}
	descriptor, err := windows.SecurityDescriptorFromString(windowsEligibilityDirectoryDACL)
	if err != nil {
		t.Fatal("fixture security descriptor unavailable")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal("fixture DACL unavailable")
	}
	err = windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, system, nil, dacl, nil)
	})
	if err != nil {
		logNativeEligibilityCause(t, err)
		t.Fatal("task-owned protected directory setup failed")
	}
	store, err := NewFileStore(filepath.Join(directory, "deferral.json"))
	if err != nil {
		t.Fatal("native store setup failed")
	}
	if err := validateParentDirectory(store.Path); err != nil {
		logNativeEligibilityCause(t, err)
		t.Fatal("protected LocalSystem fixture invalid")
	}
	return store
}

func logNativeEligibilityCause(t *testing.T, err error) {
	t.Helper()
	var native syscall.Errno
	if errors.As(err, &native) {
		t.Logf("native failure errno %d", uint64(native))
	}
}

func TestWindowsEligibilityProtectedStoreRoundTripAndCleanup(t *testing.T) {
	store := nativeEligibilityFixture(t)
	deferral := testDeferral(t)
	if err := store.Save(context.Background(), deferral); err != nil {
		logNativeEligibilityCause(t, err)
		t.Fatal("protected deferral save failed")
	}
	got, present, err := store.CurrentDeferral(context.Background())
	if err != nil || !present || got != deferral {
		logNativeEligibilityCause(t, err)
		t.Fatal("protected deferral read failed")
	}
	if err := store.Remove(context.Background()); err != nil {
		logNativeEligibilityCause(t, err)
		t.Fatal("protected deferral removal failed")
	}
	if _, present, err := store.CurrentDeferral(context.Background()); err != nil || present {
		t.Fatal("removed record remained present")
	}
	entries, err := os.ReadDir(filepath.Dir(store.Path))
	if err != nil || len(entries) != 0 {
		t.Fatal("owned staging or record artifact remained")
	}
}

func TestWindowsEligibilityNativeProbeCauseAndRecovery(t *testing.T) {
	store := nativeEligibilityFixture(t)
	checks := []func() error{
		func() error { return validateRecordPath(store.Path) },
		func() error { return secureRecordFile(store.Path) },
		func() error { return validateWindowsObjectSecurity(store.Path, windowsEligibilityRecordDACL) },
	}
	for _, check := range checks {
		err := check()
		if !errors.Is(err, ErrUnsafePath) || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatal("missing record native cause erased")
		}
	}
	if err := store.Save(context.Background(), testDeferral(t)); err != nil {
		logNativeEligibilityCause(t, err)
		t.Fatal("missing-record recovery failed")
	}
	if err := validateRecordPath(store.Path); err != nil {
		t.Fatal("recovered protected record rejected")
	}
	if err := validateWindowsObjectSecurity(store.Path, "invalid-native-descriptor"); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("descriptor probe lost policy sentinel")
	} else {
		var native syscall.Errno
		if !errors.As(err, &native) {
			t.Fatal("descriptor probe lost native cause")
		}
	}
	if err := store.Remove(context.Background()); err != nil {
		t.Fatal("owned recovery record cleanup failed")
	}
	directory := filepath.Dir(store.Path)
	if err := os.Remove(directory); err != nil {
		t.Fatal("owned parent removal failed")
	}
	err := validateParentSecurity(directory, nil)
	if !errors.Is(err, ErrUnsafePath) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing parent cause erased")
	}
}
