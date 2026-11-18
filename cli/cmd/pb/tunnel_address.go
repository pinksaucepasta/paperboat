package main

import (
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/spf13/cobra"
)

func tunnelAddressCommand() *cobra.Command {
	command := tunnelCommand("address <tunnel> <subdomain>", "Change a durable tunnel's public address", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		out, err := patchTunnel(command, args[0], api.TunnelPatchInput{Subdomain: &args[1]})
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, addressChangeMessage(out, args[1]))
	})
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

// addressChangeMessage says where the tunnel is. The server publishes the new address next to the old one and
// retires the old one only once every edge serves the new one, so right after the change (or without --wait)
// the tunnel is still at its old address.
func addressChangeMessage(out api.TunnelMutation, subdomain string) string {
	current := tunnelAddress(out.Tunnel)
	_, base, _ := strings.Cut(strings.TrimPrefix(current, "https://"), ".")
	target := "https://" + subdomain + "." + base
	if current == target || out.Operation.State == "succeeded" {
		return fmt.Sprintf("Tunnel %s is now at %s. Its previous address stops working.", out.Tunnel.ID, target)
	}
	return fmt.Sprintf("Tunnel %s is moving to %s. It keeps serving at %s until the new address is ready, then the old address stops working; follow it with pb tunnel status %s.", out.Tunnel.ID, target, current, out.Tunnel.ID)
}
