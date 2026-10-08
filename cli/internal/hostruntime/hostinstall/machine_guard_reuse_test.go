//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

func TestMachineGuardReadinessRequiresVersionAndSystemTrust(t *testing.T) {
	ready := machineguard.RuntimeStatus{Version: buildinfo.Version, Ready: true, TrustReady: true}
	if err := validateMachineGuardStatus(ready); err != nil {
		t.Fatalf("trusted guard status rejected: %v", err)
	}
	for name, status := range map[string]machineguard.RuntimeStatus{
		"not ready":     {Version: buildinfo.Version, TrustReady: true},
		"trust missing": {Version: buildinfo.Version, Ready: true},
		"old version":   {Version: "old", Ready: true, TrustReady: true},
		"renewal due":   {Version: buildinfo.Version, Ready: true, TrustReady: true, RootRenewalRequired: true},
	} {
		if err := validateMachineGuardStatus(status); err == nil {
			t.Errorf("%s status accepted", name)
		}
	}
}

func TestMachineInstallPreservesReadyMachineGuardAndValidatesSuppliedBytes(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "fixture", "custom")
	if err != nil {
		t.Fatal(err)
	}
	oldProbe, oldPrepare, oldInstall := existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard
	t.Cleanup(func() {
		existingMachineGuardReady = oldProbe
		prepareMachineGuardForInstall = oldPrepare
		installMissingMachineGuard = oldInstall
	})
	var probed, prepared, installed int
	existingMachineGuardReady = func(ctx context.Context) error {
		probed++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("guard readiness probe is not bounded")
		}
		return nil
	}
	prepareMachineGuardForInstall = func() error { prepared++; return nil }
	installMissingMachineGuard = func(context.Context, string) error { installed++; return nil }
	request := Request{Executable: executable, Source: source}
	if err := installMachineGuard(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if probed != 1 || prepared != 0 || installed != 0 {
		t.Fatal("ready shared guard was mutated")
	}
	request.Source.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := installMachineGuard(context.Background(), request); err == nil {
		t.Fatal("changed supplied bytes accepted")
	}
	if probed != 1 || prepared != 0 || installed != 0 {
		t.Fatal("invalid supplied source reached guard readiness or mutation")
	}
	request.Source = source
	existingMachineGuardReady = func(context.Context) error { probed++; return errors.New("unavailable") }
	if err := installMachineGuard(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if probed != 2 || prepared != 1 || installed != 1 {
		t.Fatal("absent guard did not use existing installation path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := installMachineGuard(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not preserved")
	}
	if probed != 2 || prepared != 1 || installed != 1 {
		t.Fatal("canceled install touched shared guard")
	}
}
