//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

func TestMachineGuardReadinessRequiresVersionButNotBrowserTrust(t *testing.T) {
	ready := machineguard.RuntimeStatus{Version: buildinfo.Version, Ready: true, TrustReady: true, DefaultBrowserDomain: "local.example.test"}
	if err := validateMachineGuardStatus(ready); err != nil {
		t.Fatalf("trusted guard status rejected: %v", err)
	}
	// Pending browser trust leaves the guard usable, so it must not force reinstallation.
	pending := machineguard.RuntimeStatus{Version: buildinfo.Version, Ready: true, DefaultBrowserDomain: "local.example.test"}
	if err := validateMachineGuardStatus(pending); err != nil {
		t.Fatalf("guard with pending browser trust rejected: %v", err)
	}
	for name, status := range map[string]machineguard.RuntimeStatus{
		"not ready":   {Version: buildinfo.Version, TrustReady: true, DefaultBrowserDomain: "local.example.test"},
		"renewal due": {Version: buildinfo.Version, Ready: true, TrustReady: true, RootRenewalRequired: true, DefaultBrowserDomain: "local.example.test"},
		"no domain":   {Version: buildinfo.Version, Ready: true, TrustReady: true},
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
	useGuardDomains(t, "", "local.example.test", nil)
	oldProbe, oldPrepare, oldInstall := existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard
	t.Cleanup(func() {
		existingMachineGuardReady = oldProbe
		prepareMachineGuardForInstall = oldPrepare
		installMissingMachineGuard = oldInstall
	})
	var probed, prepared, installed int
	existingMachineGuardReady = func(ctx context.Context, _ string) error {
		probed++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("guard readiness probe is not bounded")
		}
		return nil
	}
	prepareMachineGuardForInstall = func() error { prepared++; return nil }
	installMissingMachineGuard = func(context.Context, string, string, map[string]string) (guardUndo, error) {
		installed++
		return nil, nil
	}
	request := Request{Executable: executable, Source: source}
	if err := installMachineGuard(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
	if probed != 1 || prepared != 0 || installed != 0 {
		t.Fatal("ready shared guard was mutated")
	}
	request.Source.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := installMachineGuard(context.Background(), request, nil); err == nil {
		t.Fatal("changed supplied bytes accepted")
	}
	if probed != 1 || prepared != 0 || installed != 0 {
		t.Fatal("invalid supplied source reached guard readiness or mutation")
	}
	request.Source = source
	existingMachineGuardReady = func(context.Context, string) error { probed++; return errors.New("unavailable") }
	if err := installMachineGuard(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
	if probed != 2 || prepared != 1 || installed != 1 {
		t.Fatal("absent guard did not use existing installation path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := installMachineGuard(ctx, request, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not preserved")
	}
	if probed != 2 || prepared != 1 || installed != 1 {
		t.Fatal("canceled install touched shared guard")
	}
}

// The machine-wide guard is never downgraded by another OS user's older install: a newer release, or a
// development guard whose revision is not lower, is kept; an older one is replaced (Windows test machine). A guard fix
// that adds no feature still reaches machines through its higher revision (FIX-MULTIINST 2a602424f).
func TestMachineGuardIsNotDowngradedByAnOlderInstall(t *testing.T) {
	r := machineguard.Revision
	for name, c := range map[string]struct {
		guard    machineguard.RuntimeStatus
		version  string
		notOlder bool
	}{
		"same version":                   {machineguard.RuntimeStatus{Version: "2026.11.16.0"}, "2026.11.16.0", true},
		"newer release kept":             {machineguard.RuntimeStatus{Version: "2026.11.17.2"}, "2026.11.16.0", true},
		"older release replaced":         {machineguard.RuntimeStatus{Version: "2026.11.13.0"}, "2026.11.16.0", false},
		"dev guard of the same revision": {machineguard.RuntimeStatus{Version: "dev-a344bd52b", Revision: r}, "dev-de73faa7f", true},
		"dev guard of a higher revision": {machineguard.RuntimeStatus{Version: "dev-a344bd52b", Revision: r + 1}, "2026.11.16.0", true},
		"dev guard of a lower revision":  {machineguard.RuntimeStatus{Version: "dev-a344bd52b", Revision: r - 1, Features: machineguard.Features()}, "dev-de73faa7f", false},
	} {
		if got := MachineGuardNotOlder(c.guard, c.version); got != c.notOlder {
			t.Errorf("%s: not older = %v", name, got)
		}
	}
}
