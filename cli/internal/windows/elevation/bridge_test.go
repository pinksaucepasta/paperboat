package elevation

import (
	"testing"
	"time"
)

func TestOperationDurationBoundsRuntimeActivation(t *testing.T) {
	for _, action := range []string{ActionCommit, ActionUninstall, ActionStop, ActionConfigInstall, ActionConfigRemove, ActionDaemonInstall, ActionDaemonRemove, ActionDaemonStart, ActionDaemonStop, ActionBrowserDomain} {
		if got := operationDuration(OperationRuntimeService, action); got != RuntimeActivationDuration {
			t.Fatalf("runtime action %q duration = %s, want %s", action, got, RuntimeActivationDuration)
		}
	}
	if got := operationDuration(OperationRuntimeService, ActionRepair); got != MaxOperationDuration {
		t.Fatalf("repair duration = %s, want maintenance duration %s", got, MaxOperationDuration)
	}
	// Below the maintenance window, and above the 77 s + 60 s a real in-place update took on Windows (see the constant).
	if RuntimeActivationDuration < 150*time.Second || RuntimeActivationDuration >= MaxOperationDuration {
		t.Fatalf("invalid runtime activation duration %s", RuntimeActivationDuration)
	}
}

func TestOperationDurationIncludesInstallRecoveryAndLocalActivation(t *testing.T) {
	for _, action := range []string{ActionInstall, ActionInstallCommit} {
		if got := operationDuration(OperationRuntimeService, action); got != RuntimeInstallRecoveryDuration+RuntimeActivationDuration {
			t.Fatalf("install %q duration=%s", action, got)
		}
	}
	if RuntimeInstallRecoveryDuration != 31*time.Minute {
		t.Fatalf("recovery duration=%s", RuntimeInstallRecoveryDuration)
	}
	for _, action := range []string{ActionOpenSSHSetup, ActionOpenSSHRepair, ActionOpenSSHRemove} {
		if got := operationDuration(OperationOpenSSH, action); got != MaxOperationDuration {
			t.Fatalf("OpenSSH duration changed: %s", got)
		}
	}
}
