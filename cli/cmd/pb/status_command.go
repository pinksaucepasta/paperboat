package main

import (
	"encoding/json"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

func statusCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "status [machine]",
		Short: "Show local Paperboat machine status",
		Args:  commandArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			_, snapshot, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("read local Paperboat status: %w", err)
			}
			client, err := backendClient(actionContext(command, args))
			if err != nil {
				return err
			}
			machines, err := client.ListUserMachines(command.Context())
			if err != nil {
				return fmt.Errorf("read machines for the selected workspace: %w", friendlyCommandError(err))
			}
			allowed := make(map[string]bool, len(machines))
			for _, machine := range machines {
				allowed[machine.ID] = true
			}
			selected := make([]localapi.MachineStatus, 0, len(machines))
			for _, machine := range snapshot.Machines {
				if allowed[machine.ID] {
					selected = append(selected, machine)
				}
			}
			snapshot.Machines = selected
			if len(args) == 1 {
				machine, err := selectStatusMachine(snapshot.Machines, args[0])
				if err != nil {
					return err
				}
				snapshot.Machines = []localapi.MachineStatus{machine}
			}
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				return encoder.Encode(snapshot)
			}
			writeStatus(command.OutOrStdout(), snapshot)
			return nil
		},
	}
	command.Flags().Bool("json", false, "print JSON")
	return command
}
