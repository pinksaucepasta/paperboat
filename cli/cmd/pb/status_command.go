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
			selected, local := workspaceSnapshotMachines(command.Context(), snapshot.Machines)
			if !local {
				if selected, err = serverWorkspaceStatusMachines(command, args, snapshot.Machines); err != nil {
					return err
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

// serverWorkspaceStatusMachines keeps the snapshot machines the server lists for a team workspace.
func serverWorkspaceStatusMachines(command *cobra.Command, args []string, machines []localapi.MachineStatus) ([]localapi.MachineStatus, error) {
	client, err := backendClient(actionContext(command, args))
	if err != nil {
		return nil, err
	}
	listed, err := client.ListUserMachines(command.Context())
	if err != nil {
		return nil, fmt.Errorf("read machines for the selected workspace: %w", friendlyCommandError(err))
	}
	aliases := make(map[string]string, len(listed))
	for _, machine := range listed {
		aliases[machine.ID] = machine.Alias
	}
	selected := make([]localapi.MachineStatus, 0, len(listed))
	for _, machine := range machines {
		if alias, allowed := aliases[machine.ID]; allowed {
			machine.Alias = alias
			selected = append(selected, machine)
		}
	}
	return selected, nil
}
