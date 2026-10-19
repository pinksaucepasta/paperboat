package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// tunnelAddress is the tunnel's stable public address, shown next to its name.
// The name is only a label: renaming never changes the address.
func tunnelAddress(tunnel api.Tunnel) string {
	if tunnel.StableEndpoint == "" {
		return "-"
	}
	return tunnel.StableEndpoint
}

func tunnelListCommand() *cobra.Command {
	command := tunnelCommand("list", "List durable tunnels", cobra.NoArgs, func(command *cobra.Command, _ []string) error {
		if ephemeral, _ := command.Flags().GetBool("ephemeral"); ephemeral {
			return runPreviewListCobra(command, nil)
		}
		cursor, _ := command.Flags().GetString("cursor")
		limit, _ := command.Flags().GetInt("limit")
		if limit < 1 || limit > 200 {
			return errors.New("limit must be between 1 and 200")
		}
		if len(cursor) > 4096 || strings.ContainsAny(cursor, "\x00\r\n") {
			return errors.New("cursor is invalid")
		}
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		out, err := retryTunnelRead(ctx, func() (api.TunnelPage, error) {
			return client.ListTunnelsV1Filtered(ctx, cursor, limit, inventoryFilters(command))
		})
		if err != nil {
			return err
		}
		if jsonOut, _ := command.Flags().GetBool("json"); jsonOut {
			return tunnelOutput(command, out, "")
		}
		for _, v := range out.Items {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", v.ID, v.Name, tunnelAddress(v), v.AccessMode, v.DesiredState); err != nil {
				return err
			}
		}
		return writeInventoryContinuation(command, nil, out.NextCursor, limit)
	})
	command.Flags().String("cursor", "", "continue a previous page")
	command.Flags().Int("limit", 100, "maximum results (1-200)")
	inventoryFilterFlags(command)
	tunnelJSONFlag(command)
	return command
}

func tunnelShowCommand() *cobra.Command {
	command := tunnelCommand("show <tunnel>", "Show a durable tunnel", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
		if err != nil {
			return err
		}
		out, err := retryTunnelRead(ctx, func() (api.Tunnel, error) {
			return client.GetTunnelV1(ctx, tunnelID)
		})
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", out.ID, out.Name, tunnelAddress(out), out.AccessMode, out.DesiredState))
	})
	tunnelJSONFlag(command)
	return command
}

func tunnelStateCommand(action string) *cobra.Command {
	short := strings.ToUpper(action[:1]) + action[1:] + " a durable tunnel"
	if action == "pause" {
		short = "Pause new traffic while preserving tunnel identity and configuration"
	}
	if action == "resume" {
		short = "Resume new traffic for a preserved tunnel"
	}
	if action == "delete" {
		short = "Delete endpoints and routes and revoke connectors while preserving user DNS records"
	}
	command := tunnelCommand(action+" <tunnel>", short, cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		if action == "delete" {
			if ephemeral, _ := command.Flags().GetBool("ephemeral"); ephemeral {
				return runPreviewStopCobra(command, args)
			}
		}
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
		if err != nil {
			return err
		}
		current, err := retryTunnelRead(ctx, func() (api.Tunnel, error) {
			return client.GetTunnelV1(ctx, tunnelID)
		})
		if err != nil {
			return err
		}
		if action == "delete" {
			if err := confirmMutation(command, "tunnel-delete:"+current.ID+":"+current.ETag, fmt.Sprintf("Delete tunnel %s (%s)? Endpoints, routes, domain bindings, and connector credentials will be removed. User-owned DNS records and audit history remain.", current.Name, current.ID)); err != nil {
				return err
			}
		}
		key, err := tunnelKey()
		if err != nil {
			return err
		}
		out, err := retryTunnelRead(ctx, func() (api.TunnelMutation, error) {
			return client.ChangeTunnelStateV1(ctx, tunnelID, action, current.ETag, key)
		})
		if err != nil {
			return err
		}
		status := out.Tunnel.DesiredState
		if out.Operation.Schema != "" {
			out.Operation, err = waitForTunnelOperation(command, client, out.Operation)
			if err != nil {
				return err
			}
			status = tunnelOperationHuman(out.Operation)
		} else {
			expected := map[string]string{"delete": "deleted", "pause": "paused", "resume": "active"}[action]
			if out.Tunnel.ID != current.ID || out.Tunnel.DesiredState != expected {
				return api.ErrUnsafeTunnelResponse
			}
		}
		return tunnelOutput(command, out, fmt.Sprintf("%s tunnel %s; status: %s", tunnelActionPastTense(action), out.Tunnel.ID, status))
	})
	if action == "delete" {
		command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	}
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func tunnelActionPastTense(action string) string {
	switch action {
	case "pause":
		return "Paused"
	case "resume":
		return "Resumed"
	case "delete":
		return "Deleted"
	case "remove":
		return "Removed"
	case "verify":
		return "Verified"
	case "drain":
		return "Drained"
	case "revoke":
		return "Revoked"
	default:
		return action
	}
}
