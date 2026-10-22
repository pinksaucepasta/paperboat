package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

func teamCobraCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "team",
		Short: "Manage teams and explicit resource permissions",
		Long:  "Manage teams and explicit resource permissions. Owners appoint admins, transfer ownership, delete teams and reset ENV. Admins manage ordinary members and grants; ENV rotation requires authorized keys. Owners must transfer ownership before leaving. Removal ends team access and adopted defaults; independently granted Git access and previously received files or secrets remain. Team deletion revokes team-owned machines and preserves personal resources.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return localArgumentError("unknown team action; run `pb team --help` to list supported actions")
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	root.AddCommand(
		teamInvitationsCommand(), teamInvitationShowCommand(), teamActivityCommand(), teamReadCommand("list"), teamReadCommand("get"), teamCreateCommand(), teamInviteCommand(),
		teamAcceptCommand(), teamDeclineCommand(), teamCancelInviteCommand(), teamMutationCommand("role"),
		teamMutationCommand("remove"), teamMutationCommand("leave"), teamMutationCommand("transfer"),
		teamMutationCommand("delete"), teamGrantCommand(), teamAttachCommand(), teamMachineCommand(), teamSSOCommand(), teamSCIMCommand(),
	)
	nameSuspendedTeam(root)
	return root
}

// nameSuspendedTeam makes the leaf commands that take a team as their first argument say which team
// is suspended when the server refuses a change with team_suspended (F12-06).
func nameSuspendedTeam(command *cobra.Command) {
	subcommands := command.Commands()
	for _, sub := range subcommands {
		nameSuspendedTeam(sub)
	}
	if len(subcommands) != 0 || command.RunE == nil || !strings.Contains(command.Use, "<team>") {
		return
	}
	run := command.RunE
	command.RunE = func(c *cobra.Command, args []string) error {
		err := run(c, args)
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "team_suspended" && len(args) > 0 && validTeamCLIIdentifier(args[0]) {
			apiErr.Team = args[0]
		}
		return err
	}
}

func teamReadCommand(action string) *cobra.Command {
	use := "list"
	args := cobra.NoArgs
	if action == "get" {
		use, args = "get <team>", cobra.ExactArgs(1)
	}
	short := "List teams"
	if action == "get" {
		short = "Show one team and its current generation"
	}
	c := &cobra.Command{Use: use, Short: short, Args: commandArgs(args), RunE: func(c *cobra.Command, values []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		if action == "get" {
			if !validTeamCLIIdentifier(values[0]) {
				return localArgumentError("team must be a valid identifier")
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
			if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%d\n", team.TeamID, team.OwnerAccount, team.Generation); err != nil {
				return err
			}
		}
		return nil
	}}
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

var teamCreatePollInterval = time.Duration(config.DefaultPollIntervalSeconds) * time.Second

// startPaidTeamCreate reserves the Team and returns the Checkout that pays for
// its seats. The operation ID is derived from the request, so repeating the same
// command after a lost response reuses the same order instead of creating another.
func startPaidTeamCreate(ctx context.Context, client *api.Client, team string, seats int, interval string) (api.TeamCheckout, error) {
	configuration, err := client.ClientConfiguration(ctx)
	if err != nil {
		return api.TeamCheckout{}, err
	}
	console, err := url.Parse(strings.TrimRight(configuration.ConsoleURL, "/"))
	if err != nil || console.Host == "" || console.Scheme != "https" && console.Scheme != "http" {
		return api.TeamCheckout{}, errors.New("the Paperboat server did not publish its console address; update pb or contact the server operator")
	}
	back := console.String() + "/team"
	sum := sha256.Sum256([]byte(fmt.Sprintf("team-create\x00%s\x00%d\x00%s", team, seats, interval)))
	operation := "operation_" + hex.EncodeToString(sum[:16])
	return client.CreateTeam(ctx, api.TeamCreateRequest{OperationID: operation, TeamID: team, Seats: seats, Interval: interval, SuccessURL: back + "?created=" + url.QueryEscape(team), CancelURL: back})
}

func teamCreateCommand() *cobra.Command {
	c := &cobra.Command{Use: "create <team>", Short: "Create a team by paying for its seats", Long: "Create a team. Teams are paid for first: this prints a secure Checkout link for the seats, and the team exists as soon as payment completes. Running the same command again reuses the same order.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return localArgumentError("team must be a valid identifier")
		}
		seats, _ := c.Flags().GetInt("seats")
		interval, _ := c.Flags().GetString("interval")
		if seats < 1 || seats > 128 {
			return localArgumentError("--seats must be between 1 and 128; the owner uses one seat")
		}
		if interval != "month" && interval != "year" {
			return localArgumentError("--interval must be month or year")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		checkout, err := startPaidTeamCreate(c.Context(), client, args[0], seats, interval)
		if err != nil {
			return teamCreateFailure(err)
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		noWait, _ := c.Flags().GetBool("no-wait")
		if jsonOutput {
			if err := json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"team_id": args[0], "state": "awaiting_payment", "checkout_url": checkout.URL, "seats": seats, "interval": interval}); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(c.OutOrStdout(), "To create team %s, pay for %d seat(s) (billed per %s) here:\n  %s\nThe team is created when payment completes.\n", args[0], seats, interval, checkout.URL)
		}
		if err := openBrowser(checkout.URL); err != nil && !jsonOutput {
			fmt.Fprintln(c.ErrOrStderr(), "Could not open a browser; open the link above.")
		}
		if noWait {
			return nil
		}
		timeout, _ := c.Flags().GetDuration("wait-timeout")
		return waitForPaidTeam(c, client, args[0], timeout, jsonOutput)
	}}
	c.Flags().Int("seats", 1, "seats to buy, including the owner's")
	c.Flags().String("interval", "month", "billing interval: month or year")
	c.Flags().Bool("no-wait", false, "print the Checkout link and return without waiting for payment")
	c.Flags().Duration("wait-timeout", 15*time.Minute, "how long to wait for payment before returning")
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

// teamSeatFailure turns the server's seat_required code (not its message text)
// into the recovery the inviter can act on.
func teamSeatFailure(err error) error {
	var rejected *api.APIError
	if errors.As(err, &rejected) && rejected.Code == "seat_required" {
		return commandRejection{reason: commandRejectTeamSeatRequired}
	}
	return err
}

// teamCreateFailure explains the stable server codes a creator can act on.
func teamCreateFailure(err error) error {
	var rejected *api.APIError
	if errors.As(err, &rejected) {
		switch rejected.Code {
		case "team_reserved":
			return commandRejection{reason: commandRejectTeamReserved}
		case "validation_failed":
			return commandRejection{reason: commandRejectTeamRequestInvalid}
		}
	}
	return err
}

// waitForPaidTeam polls until the paid Team exists. Waiting is bounded; payment
// is not cancelled when it is interrupted, so the message says how to continue.
func waitForPaidTeam(c *cobra.Command, client *api.Client, team string, timeout time.Duration, jsonOutput bool) error {
	ctx, cancel := context.WithTimeout(c.Context(), timeout)
	defer cancel()
	ticker := time.NewTicker(teamCreatePollInterval)
	defer ticker.Stop()
	if !jsonOutput {
		fmt.Fprintln(c.ErrOrStderr(), "Waiting for payment... (Ctrl+C stops waiting; the team is still created once you pay)")
	}
	for {
		found, err := client.GetTeam(ctx, team)
		switch {
		case err == nil:
			return writeTeamOutput(c, jsonOutput, found)
		case !api.IsNotFound(err) && ctx.Err() == nil:
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting before team %s was created; after paying, check with `pb team get %s`", team, team)
		case <-ticker.C:
		}
	}
}

func teamInviteCommand() *cobra.Command {
	c := &cobra.Command{Use: "invite <team> <email>", Short: "Invite a member by email, including before signup", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		email := strings.TrimSpace(args[1])
		address, emailErr := mail.ParseAddress(email)
		if !validTeamCLIIdentifier(args[0]) || emailErr != nil || address.Address != email {
			return localArgumentError("use an exact team ID and a valid email address")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		invite, err := client.InviteTeamMember(c.Context(), args[0], api.TeamInviteRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, Email: email})
		if err != nil {
			return teamSeatFailure(err)
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(c.OutOrStdout()).Encode(invite)
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Invited %s to %s as member; invitation %s expires %s. Recipient must sign in and verify this email to accept.\n", invite.Email, invite.TeamID, invite.InvitationID, invite.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"))
		return err
	}}
	teamGenerationFlag(c)
	c.Flags().Bool("json", false, "print canonical JSON")
	return c
}

func teamAcceptCommand() *cobra.Command {
	c := &cobra.Command{Use: "accept <invitation>", Short: "Accept an invitation bound to this account", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return localArgumentError("invitation must be a valid identifier")
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

func teamDeclineCommand() *cobra.Command {
	c := &cobra.Command{Use: "decline <invitation>", Short: "Decline an invitation bound to this account", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return localArgumentError("invitation must be a valid identifier")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		invitation, err := client.DeclineTeamInvitation(c.Context(), args[0], api.TeamAcceptRequest{OperationID: newIdempotencyKey()})
		if err != nil {
			return err
		}
		if jsonOutput, _ := c.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(c.OutOrStdout()).Encode(invitation)
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Declined invitation %s to %s.\n", invitation.InvitationID, invitation.TeamID)
		return err
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
			return localArgumentError("team and invitation must be valid identifiers")
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
		use = "role <team> <account> <admin|billing|auditor|member>"
		exact = 3
	} else if action == "remove" || action == "transfer" {
		use = action + " <team> <account>"
		exact = 2
	}
	short := map[string]string{
		"role":     "Change a team member's role",
		"remove":   "Remove a team member",
		"leave":    "Leave a team",
		"transfer": "Transfer team ownership",
		"delete":   "Delete a team",
	}[action]
	c := &cobra.Command{Use: use, Short: short, Args: commandArgs(cobra.ExactArgs(exact)), RunE: func(c *cobra.Command, args []string) error {
		generation, err := requiredTeamGeneration(c)
		if err != nil {
			return err
		}
		for _, v := range args {
			if !validTeamCLIIdentifier(v) {
				return localArgumentError("team, account, and role values must be valid identifiers")
			}
		}
		in := api.TeamMutationRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: generation, Action: action}
		if action == "role" {
			if !slices.Contains([]string{"admin", "billing", "auditor", "member"}, args[2]) {
				return localArgumentError("role must be admin, billing, auditor or member")
			}
			in.AccountID, in.Role = args[1], args[2]
		} else if action == "remove" || action == "transfer" {
			in.AccountID = args[1]
		}
		if action == "delete" {
			confirm, _ := c.Flags().GetString("confirm")
			if confirm != args[0] {
				return localArgumentError("team deletion requires --confirm with the exact team identifier")
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
			return err
		}
		for _, v := range args[:4] {
			if !validTeamCLIIdentifier(v) {
				return localArgumentError("team, account, kind, and resource must be valid identifiers")
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
			return localArgumentError("only preview or tunnel resources may be attached")
		}
		for _, v := range args {
			if !validTeamCLIIdentifier(v) {
				return localArgumentError("team, kind, and resource must be valid identifiers")
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
		return 0, localArgumentError("--generation must be the current positive team generation")
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
	return localArgumentError("permission must be read/write for env or use/manage/inspect/replay for preview/tunnel")
}
func writeTeamOutput(c *cobra.Command, jsonOutput bool, team api.Team) error {
	if jsonOutput {
		return json.NewEncoder(c.OutOrStdout()).Encode(team)
	}
	_, err := fmt.Fprintf(c.OutOrStdout(), "TEAM\t%s\tOWNER\t%s\tGENERATION\t%d\n", team.TeamID, team.OwnerAccount, team.Generation)
	if err != nil {
		return err
	}
	if team.Name != "" {
		if _, err = fmt.Fprintf(c.OutOrStdout(), "NAME\t%s\n", team.Name); err != nil {
			return err
		}
	}
	state := "active"
	if team.Deleted {
		state = "deleted"
	} else if team.Suspended {
		state = "suspended"
	}
	if _, err = fmt.Fprintf(c.OutOrStdout(), "STATE\t%s\n", state); err != nil {
		return err
	}
	if err = writeTeamPermissions(c, team); err != nil {
		return err
	}
	if team.ENVStatus != "" {
		if _, err = fmt.Fprintf(c.OutOrStdout(), "ENV\t%s\n", team.ENVStatus); err != nil {
			return err
		}
		if team.ENVStatus == "rotation_pending" {
			if _, err = fmt.Fprintln(c.OutOrStdout(), "An owner/admin with authorized ENV keys must run pb env team rotate <team>. Access remains fenced until rotation completes."); err != nil {
				return err
			}
		}
	}
	for _, machine := range team.Machines {
		owner := machine.OwnerAccount
		ownership := "personal"
		if machine.OwnerTeamID != "" {
			owner = machine.OwnerTeamID
			ownership = "team"
		}
		if _, err = fmt.Fprintf(c.OutOrStdout(), "MACHINE\t%s\t%s\tOWNER\t%s:%s\tSTATE\t%s\tONLINE\t%t\tSHARED\t%t\n", machine.MachineID, machine.Alias, ownership, owner, machine.State, machine.Online, machine.Active); err != nil {
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

func writeTeamPermissions(c *cobra.Command, team api.Team) error {
	for _, member := range team.Members {
		if !member.Active {
			continue
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "MEMBER\t%s\t%s\n", member.AccountID, member.Role); err != nil {
			return err
		}
	}
	for _, grant := range team.Grants {
		if !grant.Active {
			continue
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "RESOURCE GRANT\t%s\t%s\t%s\t%s\n", grant.ResourceKind, grant.ResourceID, grant.AccountID, grant.Permission); err != nil {
			return err
		}
	}
	return nil
}

func teamInvitationsCommand() *cobra.Command {
	command := &cobra.Command{Use: "invitations [team]", Short: "Discover pending invitations received by you or administered for a team", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		team := ""
		if len(args) > 0 {
			team = args[0]
			if !validTeamCLIIdentifier(team) {
				return localArgumentError("use an exact team ID")
			}
		}
		client, err := backendForCommand(command)
		if err != nil {
			return err
		}
		items, err := client.TeamInvitations(command.Context(), team)
		if err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(items)
		}
		if len(items) == 0 {
			_, err = fmt.Fprintln(command.OutOrStdout(), "No pending invitations.")
			return err
		}
		for _, item := range items {
			if _, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", item.InvitationID, item.TeamID, teamInvitationRecipient(item), teamInvitationState(item), item.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")); err != nil {
				return err
			}
		}
		return nil
	}}
	command.Flags().Bool("json", false, "print canonical JSON")
	return command
}
func teamInvitationShowCommand() *cobra.Command {
	command := &cobra.Command{Use: "invitation <invitation>", Short: "Show an invitation you may receive or administer", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		if !validTeamCLIIdentifier(args[0]) {
			return localArgumentError("use an exact invitation ID")
		}
		client, err := backendForCommand(command)
		if err != nil {
			return err
		}
		item, err := client.GetTeamInvitation(command.Context(), args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(item)
	}}
	return command
}

func teamInvitationRecipient(invitation api.TeamInvitation) string {
	if invitation.Email != "" {
		return invitation.Email
	}
	return invitation.AccountID
}

func teamInvitationState(invitation api.TeamInvitation) string {
	if invitation.State == "pending" && invitation.Email != "" && invitation.AccountID == "" {
		return "pending (verify email to accept)"
	}
	return invitation.State
}
