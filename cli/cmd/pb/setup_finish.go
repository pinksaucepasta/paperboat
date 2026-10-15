package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/spf13/cobra"
)

// finishMachineSetup reports the enrollment committed by bootstrap.
func finishMachineSetup(command *cobra.Command, client *api.Client, identityStore *identity.Store, machineID, inboxPath string, jsonOutput bool) error {
	machine, err := doctorUserMachine(command.Context(), client, machineID)
	if err != nil {
		return setupFailure(fmt.Errorf("verify machine readiness: %w", err))
	}
	registration, err := identityStore.Registration()
	if err != nil {
		return setupFailure(fmt.Errorf("read completed machine registration: %w", err))
	}
	registration.InstallationGeneration, registration.UpdatedAt = machine.InstallationGeneration, time.Now().UTC()
	if err := identityStore.SaveRegistration(registration); err != nil {
		return setupFailure(fmt.Errorf("save machine registration: %w", err))
	}

	if err := exportSetupRecoveryKey(command); err != nil {
		return setupFailure(err)
	}
	if jsonOutput {
		recoveryOutput, _ := command.Flags().GetString("recovery-output")
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"machine": machine, "inbox_path": inboxPath, "recovery_output": strings.TrimSpace(recoveryOutput)})
	}
	fmt.Fprintf(command.OutOrStdout(), "Set up %s (%s)\n", machine.Alias, machine.ID)
	return nil
}
