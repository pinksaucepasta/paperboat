//go:build windows

package hostinstall

import (
	"errors"
	"strings"
	"testing"
)

func TestWindowsConfigServiceUsesProtectedOwnerInstance(t *testing.T) {
	owner := "S-1-5-21-1-2-3-1001"
	instance, err := WindowsInstanceForSID(owner)
	if err != nil {
		t.Fatal(err)
	}
	install := WindowsRuntimeConfig{OwnerSID: owner, Instance: instance, Committed: true, MachineID: "machine", StateRoot: `C:\Users\owner\runtime`}
	definition, err := windowsConfigServiceDefinition(install, owner)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Instance != instance || !strings.Contains(definition.Executable, instance) || !strings.Contains(definition.ConfigRoot, instance) {
		t.Fatalf("config service not bound to owner instance: %+v", definition)
	}
	want := []string{"daemon", "__runtime-config", "--instance", instance}
	for i := range want {
		if definition.Arguments[i] != want[i] {
			t.Fatal("worker arguments lost owner namespace")
		}
	}
	for _, change := range []func(*WindowsRuntimeConfig){
		func(v *WindowsRuntimeConfig) { v.OwnerSID = "S-1-5-21-1-2-3-1002" },
		func(v *WindowsRuntimeConfig) { v.Instance = "" },
		func(v *WindowsRuntimeConfig) { v.Committed = false },
		func(v *WindowsRuntimeConfig) { v.MachineID = "" },
	} {
		candidate := install
		change(&candidate)
		if _, err := windowsConfigServiceDefinition(candidate, owner); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("accepted different/uncommitted owner installation: %v", err)
		}
	}
}
