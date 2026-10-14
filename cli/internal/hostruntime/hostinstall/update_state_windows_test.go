//go:build windows && paperboat_native_e2e

package hostinstall

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseeligibility"
	"golang.org/x/sys/windows"
)

func TestSuppliedBundleStatePreservesReleaseEligibility(t *testing.T) {
	if !isAdministrator() {
		t.Fatal("requires an elevated Windows runner")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	request := Request{OwnerSID: user.User.Sid.String()}
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "updater")
			if existing {
				if err := ensureWindowsMachineDirectory(root, request.OwnerSID); err != nil {
					t.Fatal(err)
				}
			}
			store, err := releaseeligibility.NewFileStore(filepath.Join(root, "deferral.json"))
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := secureUpdateState(root, request); err != nil {
					t.Fatal(err)
				}
				if _, present, err := store.CurrentDeferral(context.Background()); err != nil || present {
					t.Fatalf("supplied bundle state blocks release selection without a deferral: present=%v err=%v", present, err)
				}
			}
		})
	}
}
