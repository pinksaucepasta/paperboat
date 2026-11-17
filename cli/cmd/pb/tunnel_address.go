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
	command.Long = "Change the public address of a durable tunnel selected by its exact ID or name: <subdomain> becomes the first label of its address under the same base domain: helloworld makes helloworld.<base domain>. The subdomain is 3 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen and not only digits; some names are reserved, and a name another tunnel uses is refused. The previous address stops working at once and may be taken by someone else. Routes, connectors and access policy stay attached to the tunnel, and links to the old address break, so update anything that uses it. Changes are rate limited per account."
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}
