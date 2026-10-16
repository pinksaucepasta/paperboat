package main

import (
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func tunnelRenameCommand() *cobra.Command {
	command := tunnelCommand("rename <tunnel> <name>", "Rename a durable tunnel", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		if err := validateTunnelCreateName(args[1]); err != nil {
			return err
		}
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		id, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
		if err != nil {
			return err
		}
		current, err := retryTunnelRead(ctx, func() (api.Tunnel, error) { return client.GetTunnelV1(ctx, id) })
		if err != nil {
			return err
		}
		key, err := tunnelKey()
		if err != nil {
			return err
		}
		out, err := retryTunnelRead(ctx, func() (api.TunnelMutation, error) {
			return client.PatchTunnelV1(ctx, id, current.ETag, key, api.TunnelPatchInput{Name: &args[1]})
		})
		if err != nil {
			return err
		}
		out.Operation, err = waitForTunnelOperation(command, client, out.Operation)
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("Renamed tunnel %s to %s", out.Tunnel.ID, out.Tunnel.Name))
	})
	command.Long = "Rename a durable tunnel selected by its exact ID or current name. The command reads the current resource version before changing its name and reports the server's canonical result. Existing routes, connectors and access policy remain attached to the same tunnel. Refresh and retry if another writer changed it."
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}
