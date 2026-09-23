package daemoncmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/spf13/cobra"
)

func rpcStatusCommand() *cobra.Command {
	var jsonOutput bool
	var follow bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Query daemon connection state and peer topology over gRPC IPC",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			if follow {
				ctx, cancel = context.WithCancel(cmd.Context())
			}
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			stream, err := client.StreamStatus(ctx)
			if err != nil {
				return fmt.Errorf("stream status: %w", err)
			}

			for {
				msg, err := stream.Recv()
				if err != nil {
					return err
				}

				if jsonOutput {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(msg); err != nil {
						return err
					}
				} else {
					cmd.Printf("Daemon State: %s (version: %s, generation: %d)\n",
						msg.DaemonState, msg.DaemonVersion, msg.Generation)
					if len(msg.Peers) > 0 {
						cmd.Println("\nPeers:")
						for _, p := range msg.Peers {
							tagsStr := ""
							if len(p.Tags) > 0 {
								tagsStr = fmt.Sprintf(" [tags: %s]", strings.Join(p.Tags, ", "))
							}
							onlineStr := "offline"
							if p.Online {
								onlineStr = "online"
								if p.ConnectionMode != "" && p.ConnectionMode != "unknown" {
									onlineStr = fmt.Sprintf("online (%dms, %s)", p.LatencyMs, p.ConnectionMode)
								}
							}
							cmd.Printf("  - %s (%s): %s - %s%s\n", p.Alias, p.PeerId, p.AssignedIp, onlineStr, tagsStr)
						}
					}
					if len(msg.DerpRelays) > 0 {
						cmd.Println("\nDERP Relays:")
						for _, d := range msg.DerpRelays {
							statusStr := "unreachable"
							if d.Reachable {
								statusStr = fmt.Sprintf("reachable (%dms)", d.LatencyMs)
							}
							cmd.Printf("  - %s (%s): %s\n", d.RegionCode, d.RegionName, statusStr)
						}
					}
				}

				if !follow {
					return nil
				}
			}
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON format")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream continuous status updates")
	return cmd
}

func rpcResolveCommand() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "resolve <query>",
		Short: "Resolve a peer device IP, port forwardings and tags over gRPC IPC",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			addr, err := client.ResolveDevice(ctx, args[0])
			if err != nil {
				return err
			}

			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(addr)
			}

			cmd.Printf("Device:   %s\n", addr.DeviceId)
			cmd.Printf("Alias:    %s\n", addr.Alias)
			cmd.Printf("IP:       %s\n", addr.AssignedIp)
			if len(addr.ForwardedPorts) > 0 {
				ports := make([]string, len(addr.ForwardedPorts))
				for i, p := range addr.ForwardedPorts {
					ports[i] = fmt.Sprintf("%d", p)
				}
				cmd.Printf("Ports:    %s\n", strings.Join(ports, ", "))
			}
			if len(addr.BrowserUrls) > 0 {
				ports := make([]int, 0, len(addr.BrowserUrls))
				for port := range addr.BrowserUrls {
					ports = append(ports, int(port))
				}
				sort.Ints(ports)
				cmd.Println("Browser URLs:")
				for _, port := range ports {
					cmd.Printf("  %d: %s\n", port, addr.BrowserUrls[int32(port)])
				}
			}
			if len(addr.Tags) > 0 {
				cmd.Printf("Tags:     %s\n", strings.Join(addr.Tags, ", "))
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON format")
	return cmd
}

func rpcTagCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag <device-id> <tag1> [tag2...]",
		Short: "Assign tags to a device over gRPC IPC",
		Args:  commandArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			deviceID := args[0]
			tags := args[1:]

			resp, err := client.SetDeviceTags(ctx, deviceID, tags)
			if err != nil {
				return fmt.Errorf("set device tags: %w", err)
			}

			return writeDaemonCommandResult(cmd, resp, fmt.Sprintf("Assigned tags %v to device %s (success=%v)", resp.Tags, resp.DeviceId, resp.Success))
		},
	}
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}

func rpcApproveCommand() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "approve <device-id>",
		Short: "Approve or revoke a peer device over gRPC IPC",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			deviceID := args[0]
			approved := !revoke
			resp, err := client.ApprovePeer(ctx, deviceID, approved)
			if err != nil {
				return fmt.Errorf("approve peer: %w", err)
			}

			action := "approved"
			if !approved {
				action = "revoked"
			}
			return writeDaemonCommandResult(cmd, resp, fmt.Sprintf("Peer %s %s successfully (success: %v)", resp.DeviceId, action, resp.Success))
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "revoke peer admission")
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}

// ResolveCommand creates the pb resolve command.
func ResolveCommand() *cobra.Command {
	return rpcResolveCommand()
}

// TagCommand creates the pb tag command.
func TagCommand() *cobra.Command {
	return rpcTagCommand()
}

// ApproveCommand creates the pb approve command.
func ApproveCommand() *cobra.Command {
	return rpcApproveCommand()
}
