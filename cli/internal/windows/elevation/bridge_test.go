package elevation

import (
	"testing"
	"time"
)

func TestOperationDurationBoundsRuntimeActivation(t *testing.T) {
	for _, action := range []string{ActionCommit, ActionUninstall, ActionStop, ActionConfigInstall, ActionConfigRemove, ActionBrowserDomain} {
		if got := operationDuration(OperationRuntimeService, action); got != RuntimeActivationDuration {
			t.Fatalf("runtime action %q duration = %s, want %s", action, got, RuntimeActivationDuration)
		}
	}
	if got := operationDuration(OperationRuntimeService, ActionRepair); got != MaxOperationDuration {
		t.Fatalf("repair duration = %s, want maintenance duration %s", got, MaxOperationDuration)
	}
	if RuntimeActivationDuration <= 0 || RuntimeActivationDuration >= MaxOperationDuration || RuntimeActivationDuration > 90*time.Second {
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
