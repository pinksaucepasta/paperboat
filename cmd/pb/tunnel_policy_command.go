package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

const lazyPolicyDefaultLifetime = 8 * time.Hour

type lazyPolicyClient interface {
	UpsertLazyPolicy(context.Context, api.LazyPolicyUpsertRequest) (api.LazyPolicy, error)
	GetLazyPolicy(context.Context, string) (api.LazyPolicy, error)
	DeleteLazyPolicy(context.Context, string, int64) error
}

var lazyPolicyClientForCommand = func(command *cobra.Command) (lazyPolicyClient, error) {
	return tunnelClientForCommand(command)
}

func tunnelPolicyCommand() *cobra.Command {
	root := &cobra.Command{Use: "policy", Short: "Manage permission to activate a private port on demand", Args: commandArgs(cobra.NoArgs), Long: "Manage on-demand private or team access to an exact host port. A replacement application listening on the same approved port inherits access. Paperboat does not start the application."}
	root.AddCommand(tunnelPolicyAllowCommand(), tunnelPolicyGetCommand(), tunnelPolicyRevokeCommand())
	return root
}

func tunnelPolicyAllowCommand() *cobra.Command {
	command := &cobra.Command{Use: "allow <machine> <port|url>", Short: "Allow on-demand access to an exact machine port", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
		target, err := parsePreviewTarget(args[1])
		if err != nil || target.Scheme != "http" && target.Scheme != "https" && target.Scheme != "h2c" || !lazyLoopbackAddress(target.Address) {
			return invocationError(errors.New("target must be an exact loopback HTTP, HTTPS, or h2c port"))
		}
		access, _ := command.Flags().GetString("access")
		if access != "private" && access != "team" {
			return invocationError(errors.New("access must be private or team"))
		}
		lifetime, _ := command.Flags().GetDuration("expires")
		if lifetime <= 0 {
			return invocationError(errors.New("expires must be a positive duration"))
		}
		policyID, _ := command.Flags().GetString("id")
		expected, _ := command.Flags().GetInt64("generation")
		if (policyID == "") != (expected == 0) || expected < 0 {
			return invocationError(errors.New("id and generation must be provided together when updating a policy"))
		}
		client, err := lazyPolicyClientForCommand(command)
		if err != nil {
			return err
		}
		out, err := client.UpsertLazyPolicy(command.Context(), api.LazyPolicyUpsertRequest{ID: policyID, MachineID: args[0], ExpectedGeneration: expected, Target: api.LazyPolicyTarget{Scheme: target.Scheme, Address: target.Address}, AccessMode: access, OwnershipMode: "persistent_port", ExpiresAt: time.Now().UTC().Add(lifetime)})
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("Allowed %s access at https://%s -> %s://%s until %s. Applications replacing the listener on this exact port inherit access; Paperboat will not start them.", out.AccessMode, out.Hostname, out.Target.Scheme, out.Target.Address, out.ExpiresAt.Format(time.RFC3339)))
	}, SilenceUsage: true, SilenceErrors: true}
	command.Flags().String("access", "private", "access audience: private or team")
	command.Flags().Duration("expires", lazyPolicyDefaultLifetime, "policy lifetime")
	command.Flags().String("id", "", "existing policy ID to update")
	command.Flags().Int64("generation", 0, "expected existing policy generation")
	tunnelJSONFlag(command)
	return command
}

func tunnelPolicyGetCommand() *cobra.Command {
	command := &cobra.Command{Use: "get <policy>", Short: "Show an on-demand port policy", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, err := lazyPolicyClientForCommand(command)
		if err != nil {
			return err
		}
		out, err := client.GetLazyPolicy(command.Context(), args[0])
		if err != nil {
			return err
		}
		return tunnelOutput(command, out, fmt.Sprintf("%s\t%s\t%s://%s\t%s\tgeneration %d\texpires %s", out.ID, out.Hostname, out.Target.Scheme, out.Target.Address, out.AccessMode, out.Generation, out.ExpiresAt.Format(time.RFC3339)))
	}, SilenceUsage: true, SilenceErrors: true}
	tunnelJSONFlag(command)
	return command
}

func tunnelPolicyRevokeCommand() *cobra.Command {
	command := &cobra.Command{Use: "revoke <policy>", Short: "Revoke an on-demand port policy", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, err := lazyPolicyClientForCommand(command)
		if err != nil {
			return err
		}
		expected, _ := command.Flags().GetInt64("generation")
		if expected < 0 {
			return invocationError(errors.New("generation must be positive"))
		}
		if expected == 0 {
			current, err := client.GetLazyPolicy(command.Context(), args[0])
			if err != nil {
				return err
			}
			expected = current.Generation
		}
		if err := client.DeleteLazyPolicy(command.Context(), args[0], expected); err != nil {
			return err
		}
		return tunnelOutput(command, map[string]any{"id": args[0], "revoked": true, "generation": expected}, "Revoked policy "+args[0]+".")
	}, SilenceUsage: true, SilenceErrors: true}
	command.Flags().Int64("generation", 0, "expected policy generation; omitted reads current state first")
	tunnelJSONFlag(command)
	return command
}

func lazyLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
