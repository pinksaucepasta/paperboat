package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestSetupMachineScopeRefreshPreservesIdentity(t *testing.T) {
	machineID := "machine_01234567-89ab-4cde-8fab-0123456789ab"
	previous := identity.Registration{MachineID: machineID, EnvironmentID: machineID, PublicIdentityKey: "local-key", InstallationGeneration: 7}
	current := api.UserMachine{ID: machineID, EnvironmentID: machineID, PublicIdentityKey: "local-key", InstallationGeneration: 7}
	if err := validateSetupMachineIdentity(current, &previous, "local-key"); err != nil {
		t.Fatal(err)
	}
	current.InstallationGeneration = 8
	if err := validateSetupMachineIdentity(current, &previous, "local-key"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*api.UserMachine)
	}{
		{"different machine", func(m *api.UserMachine) {
			m.ID = "machine_11234567-89ab-4cde-8fab-0123456789ab"
			m.EnvironmentID = m.ID
		}},
		{"different control scope", func(m *api.UserMachine) { m.EnvironmentID = "machine_11234567-89ab-4cde-8fab-0123456789ab" }},
		{"different installation key", func(m *api.UserMachine) { m.PublicIdentityKey = "foreign-key" }},
		{"regressed generation", func(m *api.UserMachine) { m.InstallationGeneration = 6 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := current
			change.mutate(&changed)
			if err := validateSetupMachineIdentity(changed, &previous, "local-key"); err == nil {
				t.Fatal("setup accepted a changed immutable installation binding")
			}
		})
	}
}
