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
		out, err := patchTunnel(command, args[0], api.TunnelPatchInput{Name: &args[1]})
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("Renamed tunnel %s to %s. Its address is unchanged: %s", out.Tunnel.ID, out.Tunnel.Name, tunnelAddress(out.Tunnel)))
	})
	command.Long = "Rename a durable tunnel selected by its exact ID or current name. The command reads the current resource version before changing its name and reports the server's canonical result. The name is a label: the tunnel's public address never changes. Existing routes, connectors and access policy remain attached to the same tunnel. Refresh and retry if another writer changed it."
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

// patchTunnel applies one PATCH to the tunnel selected by args[0] on its current resource version and waits for the operation.
func patchTunnel(command *cobra.Command, selector string, patch api.TunnelPatchInput) (api.TunnelMutation, error) {
	client, ctx, err := tunnelClient(command)
	if err != nil {
		return api.TunnelMutation{}, err
	}
	id, err := resolveTunnelSelectorForCommand(ctx, client, selector)
	if err != nil {
		return api.TunnelMutation{}, err
	}
	current, err := retryTunnelRead(ctx, func() (api.Tunnel, error) { return client.GetTunnelV1(ctx, id) })
	if err != nil {
		return api.TunnelMutation{}, err
	}
	key, err := tunnelKey()
	if err != nil {
		return api.TunnelMutation{}, err
	}
	out, err := retryTunnelRead(ctx, func() (api.TunnelMutation, error) {
		return client.PatchTunnelV1(tunnelHostProof(ctx, client, current), id, current.ETag, key, patch)
	})
	if err != nil {
		return api.TunnelMutation{}, err
	}
	out.Operation, err = waitForTunnelOperation(command, client, out.Operation)
	return out, err
}
