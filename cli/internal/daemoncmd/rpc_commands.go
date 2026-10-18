package daemoncmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/spf13/cobra"
)

func rpcResolveCommand() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "resolve <query>",
		Short: "Resolve a peer machine IP, port forwardings and tags over gRPC IPC",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) (resultErr error) {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return coordinatorFailure("Could not connect to the local Paperboat daemon; run pb status and retry.", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					resultErr = coordinatorFailure("Local daemon connection cleanup failed.", resultErr, closeErr)
				}
			}()

			addr, err := client.ResolveMachine(ctx, args[0])
			if err != nil {
				return coordinatorFailure("Could not resolve the machine through the local daemon; run pb status and retry.", err)
			}

			if jsonOutput {
				return coordinatorFailure("Could not write the resolved machine; retry with writable output.", json.NewEncoder(cmd.OutOrStdout()).Encode(addr))
			}

			output := bufio.NewWriter(cmd.OutOrStdout())
			fmt.Fprintf(output, "Machine:   %s\n", addr.MachineId)
			fmt.Fprintf(output, "Alias:    %s\n", addr.Alias)
			fmt.Fprintf(output, "IP:       %s\n", addr.AssignedIp)
			if len(addr.ForwardedPorts) > 0 {
				ports := make([]string, len(addr.ForwardedPorts))
				for i, p := range addr.ForwardedPorts {
					ports[i] = fmt.Sprintf("%d", p)
				}
				fmt.Fprintf(output, "Ports:    %s\n", strings.Join(ports, ", "))
			}
			if len(addr.BrowserUrls) > 0 {
				ports := make([]int, 0, len(addr.BrowserUrls))
				for port := range addr.BrowserUrls {
					ports = append(ports, int(port))
				}
				sort.Ints(ports)
				fmt.Fprintln(output, "Browser URLs:")
				for _, port := range ports {
					fmt.Fprintf(output, "  %d: %s\n", port, addr.BrowserUrls[int32(port)])
				}
			}
			if len(addr.Tags) > 0 {
				fmt.Fprintf(output, "Tags:     %s\n", strings.Join(addr.Tags, ", "))
			}
			return coordinatorFailure("Could not write the resolved machine; retry with writable output.", output.Flush())
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON format")
	return cmd
}

func rpcTagCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag <machine-id> <tag1> [tag2...]",
		Short: "Assign tags to a machine over gRPC IPC",
		Args:  commandArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) (resultErr error) {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return coordinatorFailure("Could not connect to the local Paperboat daemon; run pb status and retry.", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					resultErr = coordinatorFailure("Local daemon connection cleanup failed.", resultErr, closeErr)
				}
			}()

			machineID := args[0]
			tags := args[1:]

			resp, err := client.SetMachineTags(ctx, machineID, tags)
			if err != nil {
				return coordinatorFailure("Could not set machine tags through the local daemon; run pb status and retry.", err)
			}

			return writeDaemonCommandResult(cmd, resp, fmt.Sprintf("Assigned tags %v to machine %s (success=%v)", resp.Tags, resp.MachineId, resp.Success))
		},
	}
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}

// ErrRevokeIsMachineRevoke says where revoking a machine's enrollment lives.
var ErrRevokeIsMachineRevoke = errors.New("revoking a machine is `pb machine revoke <machine>`, which asks for confirmation")

func rpcApproveCommand() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "approve <machine-id>",
		Short: "Approve a peer machine over gRPC IPC",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) (resultErr error) {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			client, err := daemonrpc.NewClient(ctx, "")
			if err != nil {
				return coordinatorFailure("Could not connect to the local Paperboat daemon; run pb status and retry.", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					resultErr = coordinatorFailure("Local daemon connection cleanup failed.", resultErr, closeErr)
				}
			}()

			machineID := args[0]
			approved := !revoke
			resp, err := client.ApprovePeer(ctx, machineID, approved)
			if err != nil {
				return coordinatorFailure("Could not change peer approval through the local daemon; run pb status and retry.", err)
			}

			action := "approved"
			if !approved {
				action = "revoked"
			}
			return writeDaemonCommandResult(cmd, resp, fmt.Sprintf("Peer %s %s successfully (success: %v)", resp.MachineId, action, resp.Success))
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "not supported here: revoking a machine is pb machine revoke")
	_ = cmd.Flags().MarkHidden("revoke")
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
