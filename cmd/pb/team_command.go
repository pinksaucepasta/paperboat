package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func teamCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "team", Short: "Manage teams and explicit resource permissions", Long: "Manage teams and explicit resource permissions. Owners appoint admins, transfer ownership, delete teams and reset ENV. Admins manage ordinary members and grants; ENV rotation requires authorized keys. Owners must transfer ownership before leaving. Removal ends team access and adopted defaults; independently granted Git access and previously received files or secrets remain. Team deletion revokes team-owned machines and preserves personal resources.", Args: commandArgs(cobra.NoArgs)}
	root.AddCommand(
		teamActivityCommand(), teamReadCommand("list"), teamReadCommand("get"), teamCreateCommand(), teamInviteCommand(),
		teamAcceptCommand(), teamCancelInviteCommand(), teamMutationCommand("role"),
		teamMutationCommand("remove"), teamMutationCommand("leave"), teamMutationCommand("transfer"),
		teamMutationCommand("delete"), teamGrantCommand(), teamAttachCommand(), teamMachineCommand(),
	)
	return root
}

func teamReadCommand(action string) *cobra.Command {
	use := "list"
	args := cobra.NoArgs
	if action == "get" {
		use, args = "get <team>", cobra.ExactArgs(1)
	}
	c := &cobra.Command{Use: use, Short: strings.Title(action) + " teams", Args: commandArgs(args), RunE: func(c *cobra.Command, values []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		if action == "get" {
			if !validTeamCLIIdentifier(values[0]) {
				return invocationError(errors.New("team must be a valid identifier"))
			}
			team, err := client.GetTeam(c.Context(), values[0])
			if err != nil {
				return err
			}
			return writeTeamOutput(c, jsonOutput, team)
		}
		teams, err := client.ListTeams(c.Context())
		if err != nil {
			return err
		}
		sort.Slice(teams, func(i, j int) bool { return teams[i].TeamID < teams[j].TeamID })
		if jsonOutput {
			return json.NewEncoder(c.OutOrStdout()).Encode(teams)
		}
		for _, team := range teams {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%d\n", team.TeamID, team.OwnerAccount, team.Generation)
		}
		return nil
	}}
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamCreateCommand() *cobra.Command {
	c := &cobra.Command{Use: "create <team>", Short: "Create a team", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return invocationError(errors.New("team must be a valid identifier"))
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		team, err := client.CreateTeam(c.Context(), api.TeamCreateRequest{OperationID: newIdempotencyKey(), TeamID: args[0]})
		if err != nil {
			return err
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, jsonOutput, team)
	}}
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamInviteCommand() *cobra.Command {
	c := &cobra.Command{Use: "invite <team> <account>", Short: "Invite an account as a member", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		if !validTeamCLIIdentifier(args[0]) || !validTeamCLIIdentifier(args[1]) {
			return invocationError(errors.New("team and account must be valid identifiers"))
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		invite, err := client.InviteTeamMember(c.Context(), args[0], api.TeamInviteRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, AccountID: args[1]})
		if err != nil {
			return err
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(c.OutOrStdout()).Encode(invite)
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Invited %s to %s as member; invitation %s expires %s.\n", invite.AccountID, invite.TeamID, invite.InvitationID, invite.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"))
		return err
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamAcceptCommand() *cobra.Command {
	c := &cobra.Command{Use: "accept <invitation>", Short: "Accept an invitation bound to this account", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return invocationError(errors.New("invitation must be a valid identifier"))
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		team, err := client.AcceptTeamInvitation(c.Context(), args[0], api.TeamAcceptRequest{OperationID: newIdempotencyKey()})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, j, team)
	}}
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamCancelInviteCommand() *cobra.Command {
	c := &cobra.Command{Use: "cancel-invite <team> <invitation>", Short: "Cancel an outstanding invitation", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		if !validTeamCLIIdentifier(args[0]) || !validTeamCLIIdentifier(args[1]) {
			return invocationError(errors.New("team and invitation must be valid identifiers"))
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		out, err := client.CancelTeamInvitation(c.Context(), args[0], args[1], api.TeamMutationRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, Action: "cancel"})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		if j {
			return json.NewEncoder(c.OutOrStdout()).Encode(out)
		}
		return writeTeamOutput(c, false, out)
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamMutationCommand(action string) *cobra.Command {
	use := action + " <team>"
	exact := 1
	if action == "role" {
		use = "role <team> <account> <admin|member>"
		exact = 3
	} else if action == "remove" || action == "transfer" {
		use = action + " <team> <account>"
		exact = 2
	}
	c := &cobra.Command{Use: use, Short: strings.Title(action) + " team membership", Args: commandArgs(cobra.ExactArgs(exact)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		for _, v := range args {
			if !validTeamCLIIdentifier(v) {
				return invocationError(errors.New("team, account, and role values must be valid identifiers"))
			}
		}
		in := api.TeamMutationRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, Action: action}
		if action == "role" {
			if args[2] != "admin" && args[2] != "member" {
				return invocationError(errors.New("role must be admin or member"))
			}
			in.AccountID, in.Role = args[1], args[2]
		} else if action == "remove" || action == "transfer" {
			in.AccountID = args[1]
		}
		if action == "delete" {
			confirm, _ := c.Flags().GetString("confirm")
			if confirm != args[0] {
				return invocationError(errors.New("team deletion requires --confirm with the exact team identifier"))
			}
			in.Confirmation = confirm
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		team, err := client.MutateTeam(c.Context(), args[0], in)
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, j, team)
	}}
	teamGenerationFlag(c)
	if action == "delete" {
		c.Flags().String("confirm", "", "exact team identifier")
	}
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamGrantCommand() *cobra.Command {
	c := &cobra.Command{Use: "grant <team> <account> <env|preview|tunnel> <resource> <permission>", Short: "Set or revoke an explicit resource permission", Args: commandArgs(cobra.ExactArgs(5)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		active, _ := c.Flags().GetBool("active")
		if err := validateTeamResource(args[2], args[4]); err != nil {
			return invocationError(err)
		}
		for _, v := range args[:4] {
			if !validTeamCLIIdentifier(v) {
				return invocationError(errors.New("team, account, kind, and resource must be valid identifiers"))
			}
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		team, err := client.GrantTeamResource(c.Context(), args[0], api.TeamGrantRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, AccountID: args[1], ResourceKind: args[2], ResourceID: args[3], Permission: args[4], Active: active})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, j, team)
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("active", true, "grant permission; set false to revoke")
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamAttachCommand() *cobra.Command {
	c := &cobra.Command{Use: "attach <team> <preview|tunnel> <resource>", Short: "Attach or detach an explicitly selected personal resource", Args: commandArgs(cobra.ExactArgs(3)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		if args[1] != "preview" && args[1] != "tunnel" {
			return invocationError(errors.New("only preview or tunnel resources may be attached"))
		}
		for _, v := range args {
			if !validTeamCLIIdentifier(v) {
				return invocationError(errors.New("team, kind, and resource must be valid identifiers"))
			}
		}
		active, _ := c.Flags().GetBool("active")
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		team, err := client.AttachTeamResource(c.Context(), args[0], api.TeamAttachRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, ResourceKind: args[1], ResourceID: args[2], Active: active})
		if err != nil {
			return err
		}
		j, _ := c.Flags().GetBool("json")
		return writeTeamOutput(c, j, team)
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("active", true, "attach resource; set false to detach")
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamGenerationFlag(c *cobra.Command) {
	c.Flags().Uint64("generation", 0, "expected current team generation")
}
func requiredTeamGeneration(c *cobra.Command) (uint64, error) {
	g, _ := c.Flags().GetUint64("generation")
	if g == 0 {
		return 0, invocationError(errors.New("--generation must be the current positive team generation"))
	}
	return g, nil
}
func validTeamCLIIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' && r != ':' {
			return false
		}
	}
	return true
}
func validateTeamResource(kind, permission string) error {
	switch kind {
	case "env":
		if permission == "read" || permission == "write" {
			return nil
		}
	case "preview", "tunnel":
		// Inspector actions are exact-match: use/manage imply neither
		// inspect nor replay, and inspect does not imply replay.
		if permission == "use" || permission == "manage" || permission == "inspect" || permission == "replay" {
			return nil
		}
	}
	return errors.New("permission must be read/write for env or use/manage/inspect/replay for preview/tunnel")
}
func writeTeamOutput(c *cobra.Command, jsonOutput bool, team api.Team) error {
	if jsonOutput {
		return json.NewEncoder(c.OutOrStdout()).Encode(team)
	}
	_, err := fmt.Fprintf(c.OutOrStdout(), "TEAM\t%s\tOWNER\t%s\tGENERATION\t%d\n", team.TeamID, team.OwnerAccount, team.Generation)
	if err != nil {
		return err
	}
	if team.ENVStatus != "" {
		fmt.Fprintf(c.OutOrStdout(), "ENV\t%s\n", team.ENVStatus)
		if team.ENVStatus == "rotation_pending" {
			fmt.Fprintln(c.OutOrStdout(), "An owner/admin with authorized ENV keys must run pb env team rotate <team>. Access remains fenced until rotation completes.")
		}
	}
	for _, machine := range team.Machines {
		owner := machine.OwnerAccount
		ownership := "personal"
		if machine.OwnerTeamID != "" {
			owner = machine.OwnerTeamID
			ownership = "team"
		}
		if _, err = fmt.Fprintf(c.OutOrStdout(), "MACHINE\t%s\t%s\tOWNER\t%s:%s\tSTATE\t%s\tONLINE\t%t\tSHARED\t%t\n", machine.MachineID, machine.DisplayName, ownership, owner, machine.State, machine.Online, machine.Active); err != nil {
			return err
		}
	}
	for _, grant := range team.MachineGrants {
		if !grant.Active {
			continue
		}
		subject := grant.AccountID
		if grant.Audience == "all_members" {
			subject = "all members"
		}
		if _, err = fmt.Fprintf(c.OutOrStdout(), "MACHINE GRANT\t%s\t%s\t%s\n", grant.MachineID, subject, strings.Join(grant.Capabilities, ",")); err != nil {
			return err
		}
	}
	return nil
}

func teamActivityCommand() *cobra.Command {
	c := &cobra.Command{Use: "activity <team>", Short: "View owner/admin activity from the last 90 days", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		limit, _ := c.Flags().GetInt("limit")
		cursor, _ := c.Flags().GetString("cursor")
		if !validTeamCLIIdentifier(args[0]) || limit < 1 || limit > 200 {
			return invocationError(errors.New("use a valid team and limit between 1 and 200"))
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		page, err := client.TeamActivity(c.Context(), args[0], cursor, limit)
		if err != nil {
			return err
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(c.OutOrStdout()).Encode(page)
		}
		for _, item := range page.Items {
			if _, err = fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\n", item.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), item.ActorAccount, item.Action); err != nil {
				return err
			}
		}
		if page.NextCursor != "" {
			_, err = fmt.Fprintf(c.OutOrStdout(), "Next page: pb team activity %s --cursor %s --limit %d\n", args[0], page.NextCursor, limit)
		}
		return err
	}}
	c.Flags().Int("limit", 50, "maximum events, 1–200")
	c.Flags().String("cursor", "", "next_cursor from the previous page")
	c.Flags().Bool("json", false, "print canonical JSON including metadata and next_cursor")
	return c
}
