package main

import (
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func teamMachineCommand() *cobra.Command {
	c := &cobra.Command{Use: "machine", Short: "Share machines and manage exact team access", Long: "Share a personal enrollment or explicitly transfer it to a team. Each teammate uses their own PB account and starts separate terminal sessions. All remote work runs as the enrolled OS user, so it shares that user's OS file and process permissions. Tunnel management covers existing private/team tunnels; create tunnels locally on the target machine. Machine grants do not grant ENV administration, public publication, resharing, or attachment to another person's terminal.", Args: commandArgs(cobra.NoArgs)}
	for _, action := range []string{"share", "unshare", "transfer-to-team", "remove"} {
		c.AddCommand(teamMachineActionCommand(action))
	}
	c.AddCommand(teamMachineGrantCommand())
	return c
}
func teamMachineActionCommand(action string) *cobra.Command {
	descriptions := map[string]string{"share": "Share your personal machine with a team; grants are separate", "unshare": "Withdraw the team's grants while retaining personal ownership", "transfer-to-team": "Explicitly transfer your enrollment to team ownership", "remove": "Revoke a team-owned machine without personal takeover"}
	c := &cobra.Command{Use: action + " <team> <machine-id>", Short: descriptions[action], Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		if !validTeamCLIIdentifier(args[0]) || !validTeamCLIIdentifier(args[1]) {
			return invocationError(errors.New("team and machine must be valid identifiers"))
		}
		confirmation := ""
		if action == "transfer-to-team" || action == "remove" {
			confirmation, _ = c.Flags().GetString("confirm")
			if confirmation != args[1] {
				return invocationError(errors.New("ownership transfer or removal requires --confirm with the exact machine identifier"))
			}
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		out, err := client.MutateTeamMachine(c.Context(), args[0], api.TeamMachineRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, MachineID: args[1], Action: strings.ReplaceAll(action, "-", "_"), Confirmation: confirmation})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, j, out)
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("json", false, "print canonical JSON")
	if action == "transfer-to-team" || action == "remove" {
		c.Flags().String("confirm", "", "exact machine identifier acknowledging the ownership or revocation effect")
	}
	return c
}
func teamMachineGrantCommand() *cobra.Command {
	c := &cobra.Command{Use: "grant <team> <machine-id>", Short: "Set all-member or selected-member machine capabilities", Long: "Set the complete capability list for one all-member or selected-member grant. Effective access is the union of both grants. Use --active=false to revoke this grant. Team roles alone do not grant machine use.", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		all, _ := c.Flags().GetBool("all-members")
		member, _ := c.Flags().GetString("member")
		caps, _ := c.Flags().GetStringSlice("capability")
		active, _ := c.Flags().GetBool("active")
		if !validTeamCLIIdentifier(args[0]) || !validTeamCLIIdentifier(args[1]) {
			return invocationError(errors.New("team and machine must be valid identifiers"))
		}
		if all == (member != "") {
			return invocationError(errors.New("choose exactly one of --all-members or --member ACCOUNT"))
		}
		if member != "" && !validTeamCLIIdentifier(member) {
			return invocationError(errors.New("member must be an account identifier"))
		}
		if err = validateMachineGrantCapabilities(caps); err != nil {
			return invocationError(err)
		}
		audience := "selected_member"
		if all {
			audience = "all_members"
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		out, err := client.GrantTeamMachine(c.Context(), args[0], api.TeamMachineGrantRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, MachineID: args[1], Audience: audience, AccountID: member, Capabilities: caps, Active: active})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, j, out)
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("all-members", false, "grant every current and future accepted team member")
	c.Flags().String("member", "", "grant only this accepted member account")
	c.Flags().StringSlice("capability", nil, "exact capabilities: terminal,exec,managed_ssh,files,preview_manage,tunnel_manage")
	c.Flags().Bool("active", true, "set false to revoke this grant")
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}
func validateMachineGrantCapabilities(caps []string) error {
	if len(caps) < 1 || len(caps) > 6 {
		return errors.New("provide one to six exact --capability values")
	}
	seen := map[string]bool{}
	for _, capability := range caps {
		switch capability {
		case "terminal", "exec", "managed_ssh", "files", "preview_manage", "tunnel_manage":
		default:
			return errors.New("capability must be terminal, exec, managed_ssh, files, preview_manage, or tunnel_manage")
		}
		if seen[capability] {
			return errors.New("capabilities must not contain duplicates")
		}
		seen[capability] = true
	}
	return nil
}
