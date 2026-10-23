//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"testing"
)

type recordingUndo struct{ restored, discarded bool }

func (u *recordingUndo) Restore(context.Context) error { u.restored = true; return nil }
func (u *recordingUndo) Discard()                      { u.discarded = true }

// fakeGuard stands in for the machine-wide guard: its version changes when it is installed.
func useFakeGuard(t *testing.T, version, domain string, installFails bool) (*string, *recordingUndo) {
	t.Helper()
	oldReady, oldDomain, oldPrepare, oldInstall := existingMachineGuardReady, existingMachineGuardDomain, prepareMachineGuardForInstall, installMissingMachineGuard
	t.Cleanup(func() {
		existingMachineGuardReady, existingMachineGuardDomain, prepareMachineGuardForInstall, installMissingMachineGuard = oldReady, oldDomain, oldPrepare, oldInstall
	})
	running, undo := version, &recordingUndo{}
	existingMachineGuardReady = func(_ context.Context, want string) error {
		if running != want {
			return errors.New("machine guard runs " + running)
		}
		return nil
	}
	existingMachineGuardDomain = func(context.Context) string { return domain }
	prepareMachineGuardForInstall = func() error { return nil }
	installMissingMachineGuard = func(_ context.Context, _, gotDomain string) (guardUndo, error) {
		if gotDomain != domain {
			t.Fatalf("installed with domain %q, want the guard's %q", gotDomain, domain)
		}
		if installFails {
			return undo, errors.New("guard did not start")
		}
		running = "fixture"
		return undo, nil
	}
	return &running, undo
}

func TestHealMachineGuardReplacesAGuardFromThePreviousVersion(t *testing.T) {
	request := guardInstallRequest(t)
	running, undo := useFakeGuard(t, "previous", "kept.example.test", false)
	healed, err := HealMachineGuard(context.Background(), request.Source, request.Executable)
	if err != nil || !healed || *running != request.Source.Version || !undo.discarded || undo.restored {
		t.Fatalf("healed=%v err=%v guard=%q undo=%+v", healed, err, *running, undo)
	}
	healed, err = HealMachineGuard(context.Background(), request.Source, request.Executable)
	if err != nil || healed {
		t.Fatalf("matching guard was replaced again: healed=%v err=%v", healed, err)
	}
}

func TestHealMachineGuardRestoresTheOldGuardWhenReplacementFails(t *testing.T) {
	request := guardInstallRequest(t)
	running, undo := useFakeGuard(t, "previous", "kept.example.test", true)
	healed, err := HealMachineGuard(context.Background(), request.Source, request.Executable)
	if err == nil || healed || !undo.restored || *running != "previous" {
		t.Fatalf("healed=%v err=%v guard=%q undo=%+v", healed, err, *running, undo)
	}
}

func TestHealMachineGuardLeavesAMissingGuardToInstall(t *testing.T) {
	request := guardInstallRequest(t)
	useFakeGuard(t, "previous", "", false)
	installMissingMachineGuard = func(context.Context, string, string) (guardUndo, error) {
		t.Fatal("installed a guard that did not answer")
		return nil, nil
	}
	if healed, err := HealMachineGuard(context.Background(), request.Source, request.Executable); err != nil || healed {
		t.Fatalf("healed=%v err=%v", healed, err)
	}
}
