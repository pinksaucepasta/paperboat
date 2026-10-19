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

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rpcMachineFailure presents the daemon's NotFound answer as the shared unknown-machine error
// (the same message every machine-taking command gives) and keeps every other failure behind
// the generic coordinator message.
func rpcMachineFailure(message, query string, err error) error {
	if mismatch := DaemonRequestFailure(err); mismatch != err {
		return mismatch
	}
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %q", resolver.ErrMachineNotFound, query)
	}
	return coordinatorFailure(message, err)
}

func rpcResolveCommand() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "resolve <query>",
		Short: "Resolve a peer machine IP, port forwardings over gRPC IPC",
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

			account, err := CurrentAccountID(cmd)
			if err != nil {
				return coordinatorFailure("Could not read this CLI's signed-in account; run pb doctor and retry.", err)
			}
			addr, err := client.ResolveMachine(ctx, args[0], account)
			if err != nil {
				return rpcMachineFailure("Could not resolve the machine through the local daemon; run pb status and retry.", args[0], err)
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
			return coordinatorFailure("Could not write the resolved machine; retry with writable output.", output.Flush())
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON format")
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
			account, err := CurrentAccountID(cmd)
			if err != nil {
				return coordinatorFailure("Could not read this CLI's signed-in account; run pb doctor and retry.", err)
			}
			resp, err := client.ApprovePeer(ctx, machineID, approved, account)
			if mismatch := DaemonRequestFailure(err); mismatch != err {
				return mismatch
			}
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

// ApproveCommand creates the pb approve command.
func ApproveCommand() *cobra.Command {
	return rpcApproveCommand()
}

func accountLabel(account config.Account) string {
	for _, label := range []string{account.Email, account.DisplayName, account.ID} {
		if label != "" {
			return label
		}
	}
	return ""
}

// CurrentAccountID is the signed-in account of the CLI session a daemon request comes from.
func CurrentAccountID(command *cobra.Command) (string, error) {
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		return "", err
	}
	if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
		if cfg.ServerURL, err = config.NormalizeServerURL(server); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		cfg.ServerURL = buildinfo.DefaultServerURL
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return "", err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return "", err
	}
	return profile.Account.ID, nil
}

// ErrDaemonAccountMismatch is a daemon refusing a request from another account's session.
type ErrDaemonAccountMismatch struct{ Message string }

func (e ErrDaemonAccountMismatch) Error() string { return e.Message }

// DaemonRequestFailure turns the daemon's account refusal into its typed error.
func DaemonRequestFailure(err error) error {
	if status.Code(err) == codes.PermissionDenied {
		return ErrDaemonAccountMismatch{Message: status.Convert(err).Message()}
	}
	return err
}
