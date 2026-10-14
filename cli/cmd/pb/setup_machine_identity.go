package main

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// The authenticated response owns the control scope. A refresh preserves the
// local machine key and machine identity while receiving current scoped state.
func validateSetupMachineIdentity(machine api.UserMachine, previous *identity.Registration, publicKey string) error {
	if machine.ID == "" || machine.EnvironmentID != machine.ID || machine.PublicIdentityKey != publicKey || machine.InstallationGeneration < 1 ||
		previous != nil && (machine.ID != previous.MachineID || machine.InstallationGeneration < previous.InstallationGeneration) {
		return errors.New("server returned mismatched machine identity or installation generation")
	}
	return nil
}
