package main

import (
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/spf13/cobra"
)

func tunnelExtendCommand() *cobra.Command {
	command := tunnelCommand("extend <tunnel> --duration <time>", "Move a durable tunnel's expiry to now plus a duration", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		duration, _ := command.Flags().GetDuration("duration")
		if duration <= 0 {
			return localArgumentError("--duration is required and must be positive, for example --duration 24h")
		}
		expires := time.Now().UTC().Add(duration)
		out, err := patchTunnel(command, args[0], api.TunnelPatchInput{ExpiresAt: &expires})
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("Tunnel %s now expires %s.", out.Tunnel.ID, expires.Format(time.RFC3339)))
	})
	command.Long = "Set a durable tunnel's expiry to the current time plus --duration, on the tunnel's current resource version. Use it when `pb tunnel status` says the tunnel expired; the tunnel serves again once the change is applied."
	command.Flags().Duration("duration", 0, "new lifetime from now, for example 24h")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}
