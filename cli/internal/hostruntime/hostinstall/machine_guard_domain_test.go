//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// discoveredFrom is the control plane the last stubbed discovery asked.
var discoveredFrom string

// useGuardDomains stubs the guard's recorded domain and the server's discovery.
func useGuardDomains(t *testing.T, recorded, discovered string, discoverErr error) *int {
	discoveredFrom = ""
	t.Helper()
	oldExisting, oldDiscover := existingMachineGuardDomain, discoverBrowserDomain
	t.Cleanup(func() { existingMachineGuardDomain, discoverBrowserDomain = oldExisting, oldDiscover })
	discoveries := 0
	existingMachineGuardDomain = func(context.Context) string { return recorded }
	discoverBrowserDomain = func(_ context.Context, controlURL string) (string, error) {
		discoveries++
		discoveredFrom = controlURL
		return discovered, discoverErr
	}
	return &discoveries
}

func guardInstallRequest(t *testing.T) Request {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "fixture", "custom")
	if err != nil {
		t.Fatal(err)
	}
	return Request{Executable: executable, Source: source, ControlURL: "https://api.example.test"}
}

func TestGuardInstallRecordsTheServersLocalAccessDomain(t *testing.T) {
	for name, test := range map[string]struct {
		recorded, discovered, want string
		discoveries                int
	}{
		"first install learns it from the server": {"", "local.example.test", "local.example.test", 1},
		"an upgrade keeps the recorded domain":    {"kept.example.test", "other.example.test", "kept.example.test", 0},
	} {
		t.Run(name, func(t *testing.T) {
			discoveries := useGuardDomains(t, test.recorded, test.discovered, nil)
			oldReady, oldPrepare, oldInstall := existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard
			t.Cleanup(func() {
				existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard = oldReady, oldPrepare, oldInstall
			})
			existingMachineGuardReady = func(context.Context) error { return errors.New("machine guard requires upgrade") }
			prepareMachineGuardForInstall = func() error { return nil }
			var installedDomain string
			installMissingMachineGuard = func(_ context.Context, _, domain string) error { installedDomain = domain; return nil }
			if err := installMachineGuard(context.Background(), guardInstallRequest(t)); err != nil {
				t.Fatal(err)
			}
			if test.discoveries > 0 && discoveredFrom != "https://api.example.test" {
				t.Fatalf("discovery asked %q, not the installation's control plane", discoveredFrom)
			}
			if installedDomain != test.want || *discoveries != test.discoveries {
				t.Fatalf("installed domain %q after %d discoveries; want %q after %d", installedDomain, *discoveries, test.want, test.discoveries)
			}
		})
	}
}

func TestGuardInstallFailsClearlyWhenTheServerDomainIsUnavailable(t *testing.T) {
	useGuardDomains(t, "", "", errors.New("connection refused"))
	oldReady, oldPrepare, oldInstall := existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard
	t.Cleanup(func() {
		existingMachineGuardReady, prepareMachineGuardForInstall, installMissingMachineGuard = oldReady, oldPrepare, oldInstall
	})
	existingMachineGuardReady = func(context.Context) error { return errors.New("guard absent") }
	prepareMachineGuardForInstall = func() error { return nil }
	installMissingMachineGuard = func(context.Context, string, string) error {
		t.Fatal("the guard was installed without a local access domain")
		return nil
	}
	if err := installMachineGuard(context.Background(), guardInstallRequest(t)); err == nil {
		t.Fatal("an unreachable server did not stop the guard installation")
	}
}
