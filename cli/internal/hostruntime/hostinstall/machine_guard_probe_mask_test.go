//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
)

// A fresh machine's guard probe misses (no pipe/socket); that expected miss must not mask the real failure.
func TestGuardProbeMissDoesNotMaskDiscoveryFailure(t *testing.T) {
	_, invalid := config.NormalizeServerURL("")
	useGuardDomains(t, "", "", invalid)
	oldReady, oldPrepare := existingMachineGuardReady, prepareMachineGuardForInstall
	t.Cleanup(func() { existingMachineGuardReady, prepareMachineGuardForInstall = oldReady, oldPrepare })
	existingMachineGuardReady = func(context.Context, string, int) error {
		return &fs.PathError{Op: "open", Path: `\\.\pipe\PaperboatMachineGuard`, Err: fs.ErrNotExist}
	}
	prepareMachineGuardForInstall = func() error { return nil }
	err := installMachineGuard(context.Background(), guardInstallRequest(t), nil)
	if err == nil || errors.Is(err, fs.ErrNotExist) || !errors.Is(err, invalid) {
		t.Fatalf("install error = %v; want the discovery failure without the probe miss", err)
	}
	if f := errorreport.ProjectFault(context.Background(), "hostinstall", "install", "guard", "operation_failed", err); f.Cause == "not_found" || f.Errno == 2 {
		t.Fatalf("fault cause %q errno %d; the probe miss masked the real cause", f.Cause, f.Errno)
	}
}

// A probe failure that is not a plain miss stays attached as a cause.
func TestGuardProbeFailureStaysACause(t *testing.T) {
	discoverErr := errors.New("discovery failed")
	useGuardDomains(t, "", "", discoverErr)
	oldReady, oldPrepare := existingMachineGuardReady, prepareMachineGuardForInstall
	t.Cleanup(func() { existingMachineGuardReady, prepareMachineGuardForInstall = oldReady, oldPrepare })
	probeErr := errors.New("machine guard requires upgrade")
	existingMachineGuardReady = func(context.Context, string, int) error { return probeErr }
	prepareMachineGuardForInstall = func() error { return nil }
	if err := installMachineGuard(context.Background(), guardInstallRequest(t), nil); !errors.Is(err, probeErr) || !errors.Is(err, discoverErr) {
		t.Fatalf("install error = %v; want both causes", err)
	}
}

func TestValidateControlURLRefusesEmptyAndInvalid(t *testing.T) {
	if err := ValidateControlURL("  "); !errors.Is(err, ErrNoServerURL) || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("empty URL: %v", err)
	}
	for _, raw := range []string{"not a url", "ftp://api.example.test", "https://api.example.test/console"} {
		if err := ValidateControlURL(raw); err == nil || !strings.Contains(err.Error(), "--server") {
			t.Fatalf("%q: %v", raw, err)
		}
	}
	if err := ValidateControlURL("https://api.example.test"); err != nil {
		t.Fatal(err)
	}
}
