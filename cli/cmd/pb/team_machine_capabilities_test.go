package main

import "testing"

func TestMachineGrantAcceptsEveryServerCapabilityIncludingPrivateAccess(t *testing.T) {
	if err := validateMachineGrantCapabilities([]string{"terminal", "exec", "managed_ssh", "files", "preview_manage", "tunnel_manage", "private_access"}); err != nil {
		t.Fatalf("a full grant was refused: %v", err)
	}
	if err := validateMachineGrantCapabilities([]string{"private_access"}); err != nil {
		t.Fatalf("private_access was refused: %v", err)
	}
	for name, caps := range map[string][]string{"none": nil, "unknown": {"private"}, "duplicate": {"files", "files"}} {
		if err := validateMachineGrantCapabilities(caps); err == nil {
			t.Fatalf("%s capability list was accepted", name)
		}
	}
}
