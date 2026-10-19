package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func tunnelConnectorCommand() *cobra.Command {
	root := tunnelCommand("connector", "Manage tunnel connectors", cobra.NoArgs, nil)
	root.AddCommand(connectorListCommand(), connectorAddCommand())
	for _, a := range []string{"drain", "revoke"} {
		root.AddCommand(connectorMutationCommand(a))
	}
	return root
}

var ErrTunnelConnectorAddRequiresRuntime = errors.New("connector add requires the installed Paperboat runtime")

// tunnelConnectorAddRuntime is installed by the stable-runtime integration.
// The CLI surface never consumes or prints enrollment tokens itself.
var tunnelConnectorAddRuntime = runProductionTunnelConnectorAdd

func connectorAddCommand() *cobra.Command {
	command := tunnelCommand("add <tunnel>", "Add a connector on this host", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		if tunnelConnectorAddRuntime == nil {
			return ErrTunnelConnectorAddRequiresRuntime
		}
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
		if err != nil {
			return err
		}
		return tunnelConnectorAddRuntime(command, tunnelID)
	})
	tunnelJSONFlag(command)
	return command
}

func connectorListCommand() *cobra.Command {
	command := tunnelCommand("list <tunnel>", "List tunnel connectors", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		cursor, limit, e := tunnelPageFlags(command)
		if e != nil {
			return e
		}
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelConnectorPage, error) {
			return c.ListTunnelConnectorsV1(ctx, tunnelID, cursor, limit)
		})
		if e != nil {
			return e
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return tunnelOutput(command, out, "")
		}
		for _, connector := range out.Items {
			lastHeartbeat := "never"
			if connector.LastHeartbeatAt != nil {
				lastHeartbeat = connector.LastHeartbeatAt.UTC().Format(time.RFC3339)
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", connector.ID, connector.HostID, connector.DesiredState, connector.DrainState, lastHeartbeat); err != nil {
				return err
			}
		}
		return writeInventoryContinuation(command, args, out.NextCursor, limit)
	})
	tunnelPageFlag(command)
	tunnelJSONFlag(command)
	return command
}

func connectorMutationCommand(action string) *cobra.Command {
	command := tunnelCommand(action+" <tunnel> <connector>", strings.ToUpper(action[:1])+action[1:]+" a tunnel connector", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		current, e := retryTunnelRead(ctx, func() (api.TunnelConnector, error) {
			return c.GetTunnelConnectorV1(ctx, tunnelID, args[1])
		})
		if e != nil {
			return e
		}
		if action == "revoke" {
			if e := confirmMutation(command, "tunnel-connector-revoke:"+tunnelID+":"+current.ID+":"+current.ETag, fmt.Sprintf("Revoke connector %s on tunnel %s? This host attachment will be disabled; other connectors remain.", current.ID, tunnelID)); e != nil {
				return e
			}
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelConnectorMutation, error) {
			return c.MutateTunnelConnectorV1(ctx, tunnelID, args[1], action, current.ETag, key)
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		human := fmt.Sprintf("%s connector %s; tunnel identity is preserved; status: %s", tunnelActionPastTense(action), out.Connector.ID, tunnelOperationHuman(out.Operation))
		return tunnelOutput(command, out, human)
	})
	if action == "revoke" {
		command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	}
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func tunnelCredentialsCommand() *cobra.Command {
	root := tunnelCommand("credentials", "Manage tunnel credentials", cobra.NoArgs, nil)
	root.AddCommand(credentialsRotateCommand())
	return root
}

func credentialsRotateCommand() *cobra.Command {
	command := tunnelCommand("rotate <tunnel>", "Rotate tunnel connector credentials", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		current, e := retryTunnelRead(ctx, func() (api.Tunnel, error) {
			return c.GetTunnelV1(ctx, tunnelID)
		})
		if e != nil {
			return e
		}
		if e := confirmMutation(command, "tunnel-credentials-rotate:"+current.ID+":"+current.ETag, fmt.Sprintf("Rotate credentials for tunnel %s (%s)? Existing connector credentials will be revoked and attachments may need to reconnect.", current.Name, current.ID)); e != nil {
			return e
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelOperation, error) {
			return c.RotateTunnelCredentialsV1(ctx, tunnelID, current.ETag, key)
		})
		if e != nil {
			return e
		}
		out, e = waitForTunnelOperation(command, c, out)
		if e != nil {
			return e
		}
		return tunnelOutput(command, out, fmt.Sprintf("Rotated tunnel credentials; tunnel identity and routes are preserved; status: %s", tunnelOperationHuman(out)))
	})
	command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}
