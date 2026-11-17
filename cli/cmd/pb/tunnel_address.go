package main

import (
	"fmt"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/spf13/cobra"
)

func tunnelAddressCommand() *cobra.Command {
	command := tunnelCommand("address <tunnel> <subdomain>", "Change a durable tunnel's public address", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		out, err := patchTunnel(command, args[0], api.TunnelPatchInput{Subdomain: &args[1]})
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("Tunnel %s is now at %s. Its previous address stops working.", out.Tunnel.ID, tunnelAddress(out.Tunnel)))
	})
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}
