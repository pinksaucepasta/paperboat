//go:build windows

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// Exercises recovery through its real filesystem owner boundary, without SCM
// mutations. The prior prototype created inherited BA-owned empty lock roots.
func TestNativeWindowsLifecycleProtectsUnusedRoot(t *testing.T) {
	owner, descriptor, err := lifecycleSecurity()
	if err != nil {
		t.Fatal(err)
	}
	if owner.String() != "S-1-5-18" {
		t.Skip("requires an elevated administrator or SYSTEM")
	}
	ba, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	apply := func(path, dacl string, sid *windows.SID) {
		t.Helper()
		sd, err := windows.SecurityDescriptorFromString(dacl)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sid, nil, acl, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, scenario := range []string{"fresh", "empty-inherited", "journal", "nonempty-lock", "unknown-neighbor", "mutable-lock", "mutable-directory", "foreign-owner", "linked-lock"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			apply(root, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)", owner)
			state := filepath.Join(root, "service-lifecycle")
			lock := filepath.Join(state, "service-lifecycle.lock")
			if scenario != "fresh" {
				if err := os.Mkdir(state, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lock, nil, 0600); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{state, lock} {
					if err := windowssecurity.WithRestorePrivilege(func() error {
						return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, ba, nil, nil, nil)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch scenario {
			case "journal":
				if err := os.WriteFile(filepath.Join(state, "service-lifecycle.json"), []byte("untrusted journal bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nonempty-lock":
				if err := os.WriteFile(lock, []byte("unexpected authority"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-neighbor":
				if err := os.WriteFile(filepath.Join(state, "other"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "mutable-lock", "mutable-directory":
				path := lock
				if scenario == "mutable-directory" {
					path = state
				}
				apply(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;BU)", ba)
			case "foreign-owner":
				foreign, _ := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
				apply(state, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)", foreign)
			case "linked-lock":
				if err := os.Link(lock, filepath.Join(root, "outside-lock")); err != nil {
					t.Fatal(err)
				}
			}
			component := newFakeTransactionalComponent("hostd", nil, nil)
			manager, err := NewLifecycleManager(LifecycleConfig{StateRoot: state, Components: []TransactionalComponent{component}})
			if err != nil {
				t.Fatal(err)
			}
			err = manager.Recover(context.Background())
			accepted := scenario == "fresh" || scenario == "empty-inherited"
			if accepted && err != nil || !accepted && !errors.Is(err, ErrLifecycleInvalid) {
				t.Fatalf("recovery error = %v; accepted = %v", err, accepted)
			}
			if accepted {
				for _, path := range []string{state, lock} {
					if !windowssecurity.OwnerMatchesSID(path, owner) || !windowssecurity.ProtectedDACLMatches(path, descriptor) {
						t.Fatalf("recovery did not establish exact native custody for %s", filepath.Base(path))
					}
				}
			} else if windowssecurity.OwnerMatchesSID(state, owner) && windowssecurity.ProtectedDACLMatches(state, descriptor) {
				t.Fatal("rejected unused-root repair changed directory custody")
			}
		})
	}
}
