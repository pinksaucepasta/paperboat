//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

func TestMachineInstallSucceedsWhenGuardTrustIsPending(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "fixture", "custom")
	if err != nil {
		t.Fatal(err)
	}
	useGuardDomains(t, "", "local.example.test", nil)
	oldProbe, oldPrepare, oldInstall := existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard
	t.Cleanup(func() {
		existingMachineGuardReady = oldProbe
		prepareMachineGuardForInstall = oldPrepare
		installMissingMachineGuard = oldInstall
	})
	existingMachineGuardReady = func(context.Context) error { return errors.New("guard absent") }
	prepareMachineGuardForInstall = func() error { return nil }
	request := Request{Executable: executable, Source: source}

	installMissingMachineGuard = func(context.Context, string, string) (guardUndo, error) {
		return nil, fmt.Errorf("%w: approval denied", machineguard.ErrTrustPending)
	}
	if err := installMachineGuard(context.Background(), request); err != nil {
		t.Fatalf("pending browser trust failed the machine guard installation: %v", err)
	}

	cause := errors.New("guard copy failed")
	installMissingMachineGuard = func(context.Context, string, string) (guardUndo, error) { return nil, cause }
	if err := installMachineGuard(context.Background(), request); !errors.Is(err, cause) {
		t.Fatalf("real guard installation failure was not preserved: %v", err)
	}
}
