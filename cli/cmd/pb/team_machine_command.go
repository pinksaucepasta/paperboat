package main

import (
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func teamMachineCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "machine",
		Short: "Share machines and manage exact team access",
		Long:  "Share a personal enrollment or explicitly transfer it to a team. Each teammate uses their own PB account and starts separate terminal sessions. All remote work runs as the enrolled OS user, so it shares that user's OS file and process permissions. Tunnel management covers existing private/team tunnels; create tunnels locally on the target machine. Machine grants do not grant ENV administration, public publication, resharing, or attachment to another person's terminal.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return localArgumentError("unknown team machine action; run `pb team machine --help` to list supported actions")
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	for _, action := range []string{"share", "unshare", "transfer-to-team", "remove"} {
		c.AddCommand(teamMachineActionCommand(action))
	}
	for _, action := range []string{"approve", "decline"} {
		c.AddCommand(teamMachineDecisionCommand(action))
	}
	c.AddCommand(teamMachineRequestsCommand(), teamMachineApprovalCommand(), teamMachineGrantCommand())
	return c
}
func teamMachineActionCommand(action string) *cobra.Command {
	descriptions := map[string]string{"share": "Share your personal machine with a team; grants are separate. A team that approves new machines holds it until an owner or admin accepts it", "unshare": "Withdraw the team's grants while retaining personal ownership", "transfer-to-team": "Explicitly transfer your enrollment to team ownership", "remove": "Revoke a team-owned machine without personal takeover"}
	c := &cobra.Command{Use: action + " <team> <machine-id>", Short: descriptions[action], Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		if !validTeamCLIIdentifier(args[0]) || !validTeamCLIIdentifier(args[1]) {
			return localArgumentError("team and machine must be valid identifiers")
		}
		confirmation := ""
		if action == "transfer-to-team" || action == "remove" {
			confirmation, _ = c.Flags().GetString("confirm")
			if confirmation != args[1] {
				return localArgumentError("ownership transfer or removal requires --confirm with the exact machine identifier")
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
		if err := writeTeamOutput(c, j, out); err != nil {
			return err
		}
		if action == "remove" && !j {
			_, err := fmt.Fprintln(c.OutOrStdout(), "If this device was lost or compromised, rotate ENV keys.")
			return err
		}
		return nil
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
			return localArgumentError("team and machine must be valid identifiers")
		}
		if all == (member != "") {
			return localArgumentError("choose exactly one of --all-members or --member ACCOUNT")
		}
		if member != "" && !validTeamCLIIdentifier(member) {
			return localArgumentError("member must be an account identifier")
		}
		if err = validateMachineGrantCapabilities(caps); err != nil {
			return err
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
		return localArgumentError("provide one to six exact --capability values")
	}
	seen := map[string]bool{}
	for _, capability := range caps {
		switch capability {
		case "terminal", "exec", "managed_ssh", "files", "preview_manage", "tunnel_manage":
		default:
			return localArgumentError("capability must be terminal, exec, managed_ssh, files, preview_manage, or tunnel_manage")
		}
		if seen[capability] {
			return localArgumentError("capabilities must not contain duplicates")
		}
		seen[capability] = true
	}
	return nil
}

// teamMachineDecisionCommand lets an owner or admin accept or decline a machine a member asked to add.
func teamMachineDecisionCommand(action string) *cobra.Command {
	short := map[string]string{"approve": "Accept a machine a member asked to add to the team", "decline": "Decline a machine a member asked to add to the team"}[action]
	c := &cobra.Command{Use: action + " <team> <machine-id>", Short: short, Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) || !validTeamCLIIdentifier(args[1]) {
			return localArgumentError("team and machine must be valid identifiers")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		generation, _ := c.Flags().GetUint64("generation")
		if generation == 0 {
			current, err := client.GetTeam(c.Context(), args[0])
			if err != nil {
				return err
			}
			generation = current.Generation
		}
		out, err := client.MutateTeamMachine(c.Context(), args[0], api.TeamMachineRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, MachineID: args[1], Action: action})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		if j {
			return writeTeamOutput(c, j, out)
		}
		verb := map[string]string{"approve": "Accepted", "decline": "Declined"}[action]
		_, err = fmt.Fprintf(c.OutOrStdout(), "%s machine %s for team %s.\n", verb, args[1], args[0])
		return err
	}}
	c.Flags().Uint64("generation", 0, "expected current team generation (default: the current one)")
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

// teamMachineRequestsCommand lists machines waiting for the team's owners and admins.
func teamMachineRequestsCommand() *cobra.Command {
	c := &cobra.Command{Use: "requests <team>", Short: "List machines waiting for a team owner or admin", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return localArgumentError("team must be a valid identifier")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		team, err := client.GetTeam(c.Context(), args[0])
		if err != nil {
			return err
		}
		if j, _ := c.Flags().GetBool("json"); j {
			return writeCLIJSON(c.OutOrStdout(), team.MachineRequests)
		}
		if len(team.MachineRequests) == 0 {
			_, err = fmt.Fprintln(c.OutOrStdout(), "No machines are waiting.")
			return err
		}
		for _, request := range team.MachineRequests {
			if _, err = fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\tfrom %s\n", request.MachineID, request.Alias, request.State, request.RequestedBy); err != nil {
				return err
			}
		}
		return nil
	}}
	c.Flags().Bool("json", false, "print JSON")
	return c
}

// teamMachineApprovalCommand turns the team's "approve new machines" rule on or off.
func teamMachineApprovalCommand() *cobra.Command {
	c := &cobra.Command{Use: "approval <team> <on|off>", Short: "Set whether machines members add wait for an owner or admin", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) || (args[1] != "on" && args[1] != "off") {
			return localArgumentError("use: pb team machine approval <team> <on|off>")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		current, err := client.GetTeam(c.Context(), args[0])
		if err != nil {
			return err
		}
		on := args[1] == "on"
		if _, err = client.MutateTeam(c.Context(), args[0], api.TeamMutationRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: current.Generation, Action: "machine_approval", ApproveNewMachines: &on}); err != nil {
			return err
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Machines members add to %s now %s.\n", args[0], map[bool]string{true: "wait for an owner or admin", false: "join right away"}[on])
		return err
	}}
	return c
}
