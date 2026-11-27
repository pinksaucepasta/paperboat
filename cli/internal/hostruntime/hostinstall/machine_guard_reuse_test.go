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
	existingMachineGuardReady = func(ctx context.Context, _ string, _ int) error {
		probed++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("guard readiness probe is not bounded")
		}
		return nil
	}
	prepareMachineGuardForInstall = func() error { prepared++; return nil }
	installMissingMachineGuard = func(context.Context, string, string, *machineguard.UnitSet) (guardUndo, error) {
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
	existingMachineGuardReady = func(context.Context, string, int) error { probed++; return errors.New("unavailable") }
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
		revision int // the candidate build's guard revision; 0 means r (this build's)
	}{
		// 2026-11-27: official pb 2026.11.26.21 running `pb update --from <dev build>` kept its own guard because the
		// decision used the running installer's revision, not the candidate's.
		"official guard replaced by a dev build of a newer revision": {machineguard.RuntimeStatus{Version: "2026.11.26.21", Revision: r}, "dev-x", false, r + 1},
		"official guard kept by a dev build of the same revision":    {machineguard.RuntimeStatus{Version: "2026.11.26.21", Revision: r}, "dev-x", true, r},
		"dev guard of a higher revision kept by an older dev build":  {machineguard.RuntimeStatus{Version: "dev-a", Revision: r + 1}, "dev-x", true, r},
		"dev guard replaced by a release of a newer revision":        {machineguard.RuntimeStatus{Version: "dev-a", Revision: r + 1}, "2026.11.16.0", false, r + 2},
		"same version":                    {machineguard.RuntimeStatus{Version: "2026.11.16.0"}, "2026.11.16.0", true, 0},
		"newer release kept":              {machineguard.RuntimeStatus{Version: "2026.11.17.2"}, "2026.11.16.0", true, 0},
		"older release replaced":          {machineguard.RuntimeStatus{Version: "2026.11.13.0"}, "2026.11.16.0", false, 0},
		"dev guard of the same revision":  {machineguard.RuntimeStatus{Version: "dev-a344bd52b", Revision: r}, "dev-de73faa7f", true, 0},
		"dev guard of a higher revision":  {machineguard.RuntimeStatus{Version: "dev-a344bd52b", Revision: r + 1}, "2026.11.16.0", true, 0},
		"dev guard replaced by a release": {machineguard.RuntimeStatus{Version: "dev-76b5e9764", Revision: r}, "2026.11.16.0", false, 0},
		"dev guard of a lower revision":   {machineguard.RuntimeStatus{Version: "dev-a344bd52b", Revision: r - 1, Features: machineguard.Features()}, "dev-de73faa7f", false, 0},
	} {
		revision := c.revision
		if revision == 0 {
			revision = r
		}
		if got := MachineGuardNotOlder(c.guard, c.version, revision); got != c.notOlder {
			t.Errorf("%s: not older = %v", name, got)
		}
	}
}

// The installing process may be another (older) build: readiness is judged against the candidate units' revision.
func TestCandidateGuardRevisionIsTheUnits(t *testing.T) {
	if got := candidateGuardRevision(&machineguard.UnitSet{Revision: machineguard.Revision + 5}); got != machineguard.Revision+5 {
		t.Fatalf("candidate revision = %d", got)
	}
	if got := candidateGuardRevision(nil); got != machineguard.Revision {
		t.Fatalf("own build revision = %d", got)
	}
	if got := candidateGuardRevision(&machineguard.UnitSet{}); got != 0 {
		t.Fatalf("unknown revision = %d", got)
	}
	// An unknown candidate (Windows, the heal) keeps only its own version or a release not older.
	official := machineguard.RuntimeStatus{Version: "2026.11.26.21", Revision: machineguard.Revision}
	for version, keep := range map[string]bool{"dev-x": false, "2026.11.26.21": true, "2026.11.26.20": true, "2026.11.27.0": false} {
		if got := MachineGuardNotOlder(official, version, 0); got != keep {
			t.Errorf("unknown candidate %s: keep = %v", version, got)
		}
	}
	if MachineGuardNotOlder(machineguard.RuntimeStatus{Version: "dev-a", Revision: machineguard.Revision + 9}, "2026.11.27.0", 0) {
		t.Error("unknown release candidate kept a dev guard")
	}
}
