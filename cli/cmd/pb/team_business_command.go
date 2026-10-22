package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// Team business features (PLAN F19): the audit log (`pb team activity`), SSO (`pb team sso`) and
// directory sync (`pb team scim`). Reference: server/docs/teams-business.md.

func teamActivityCommand() *cobra.Command {
	c := &cobra.Command{Use: "activity <team>", Short: "Search the team audit log (owners, admins, auditors)", Long: "Search the team audit log, newest first. Owners, admins and auditors can read it. Filter by actor (account ID, email or API token), action or action prefix, target, result (success or denied) and time range, or search text. Each event shows when, who, what, the result and the target. Use --json for the documented event shape and the next page cursor.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		filters, err := activityFilters(c, args[0])
		if err != nil {
			return err
		}
		limit, _ := c.Flags().GetInt("limit")
		cursor, _ := c.Flags().GetString("cursor")
		if limit < 1 || limit > 200 {
			return localArgumentError("use a valid team and limit between 1 and 200")
		}
		filters.Set("limit", strconv.Itoa(limit))
		if cursor != "" {
			filters.Set("cursor", cursor)
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		page, err := client.Activity(c.Context(), args[0], filters)
		if err != nil {
			return err
		}
		if jsonOutput, _ := c.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(c.OutOrStdout()).Encode(page)
		}
		for _, e := range page.Items {
			actor := e.Actor.Email
			if actor == "" {
				actor = e.Actor.Kind
			}
			line := fmt.Sprintf("%s\t%s\t%s\t%s", e.OccurredAt.Format(time.RFC3339), actor, e.Action, e.Result)
			if e.Reason != "" {
				line += " (" + e.Reason + ")"
			}
			if e.Target.Name != "" {
				line += "\t" + e.Target.Name
			}
			if _, err = fmt.Fprintln(c.OutOrStdout(), line); err != nil {
				return err
			}
		}
		if page.NextCursor != "" {
			_, err = fmt.Fprintf(c.OutOrStdout(), "Next page: pb team activity %s --cursor %s --limit %d\n", args[0], page.NextCursor, limit)
		}
		return err
	}}
	activityFilterFlags(c)
	c.Flags().Int("limit", 50, "maximum events, 1–200")
	c.Flags().String("cursor", "", "next_cursor from the previous page")
	c.Flags().Bool("json", false, "print the events as JSON, with next_cursor")
	c.AddCommand(teamActivityExportCommand())
	return c
}

func teamActivityExportCommand() *cobra.Command {
	c := &cobra.Command{Use: "export <team>", Short: "Export the team audit log as CSV or JSON", Long: "Download every audit log event that matches the filters, newest first, as a CSV file or a JSON array, for an archive or a SIEM. The same filters as pb team activity apply. The file is written to a temporary name and renamed when complete, so a failed download never leaves a partial file. Each export is itself recorded in the audit log.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		filters, err := activityFilters(c, args[0])
		if err != nil {
			return err
		}
		format, _ := c.Flags().GetString("format")
		output, _ := c.Flags().GetString("output")
		if format != "csv" && format != "json" {
			return localArgumentError("--format must be csv or json")
		}
		if output == "" {
			return localArgumentError("choose a file with -o <file>, or -o - for standard output")
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		if output == "-" {
			return client.ExportActivity(c.Context(), args[0], format, filters, c.OutOrStdout())
		}
		// Write next to the target and rename, so a failed download never leaves a partial file.
		tmp, err := os.CreateTemp(dirOf(output), ".pb-audit-*")
		if err != nil {
			return fmt.Errorf("create the export file: %w", err)
		}
		defer os.Remove(tmp.Name())
		if err = client.ExportActivity(c.Context(), args[0], format, filters, tmp); err != nil {
			return errors.Join(err, tmp.Close())
		}
		if err = tmp.Chmod(0o600); err != nil {
			return errors.Join(err, tmp.Close())
		}
		if err = tmp.Close(); err != nil {
			return err
		}
		if err = os.Rename(tmp.Name(), output); err != nil {
			return fmt.Errorf("save the export as %s: %w", output, err)
		}
		_, err = fmt.Fprintf(c.ErrOrStderr(), "Saved the audit log to %s\n", output)
		return err
	}}
	activityFilterFlags(c)
	c.Flags().String("format", "csv", "csv or json")
	c.Flags().StringP("output", "o", "", "file to write, or - for standard output")
	return c
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[:i+1]
	}
	return "."
}

func activityFilterFlags(c *cobra.Command) {
	c.Flags().String("actor", "", "account ID, email or API token ID")
	c.Flags().String("action", "", "action, or a prefix such as team or billing")
	c.Flags().String("target", "", "target resource or machine ID")
	c.Flags().String("result", "", "success or denied")
	c.Flags().String("since", "", "start: RFC 3339 time or a duration ago such as 24h or 7d")
	c.Flags().String("until", "", "end: RFC 3339 time or a duration ago")
	c.Flags().String("search", "", "text in the action, target name or actor email")
}

func activityFilters(c *cobra.Command, team string) (url.Values, error) {
	if !validTeamCLIIdentifier(team) {
		return nil, localArgumentError("use a valid team and limit between 1 and 200")
	}
	out := url.Values{}
	for flag, param := range map[string]string{"actor": "actor", "action": "action", "target": "target", "result": "result", "search": "q"} {
		if v, _ := c.Flags().GetString(flag); v != "" {
			out.Set(param, v)
		}
	}
	if r := out.Get("result"); r != "" && r != "success" && r != "denied" {
		return nil, localArgumentError("--result must be success or denied")
	}
	for _, flag := range []string{"since", "until"} {
		v, _ := c.Flags().GetString(flag)
		if v == "" {
			continue
		}
		at, err := activityTime(v, time.Now())
		if err != nil {
			return nil, localArgumentError("--" + flag + " takes an RFC 3339 time or a duration such as 24h or 7d")
		}
		out.Set(flag, at.UTC().Format(time.RFC3339))
	}
	return out, nil
}

// activityTime reads an RFC 3339 time or a duration ago ("90m", "24h", "7d").
func activityTime(value string, now time.Time) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 || n > 3650 {
			return time.Time{}, errors.New("invalid days")
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return time.Time{}, errors.New("invalid duration")
	}
	return now.Add(-d), nil
}

func teamSSOCommand() *cobra.Command {
	root := &cobra.Command{Use: "sso", Short: "Single sign-on for a team (owners and admins)", Long: "Single sign-on for a team through WorkOS, with SAML or OpenID Connect and any identity provider such as Okta, Entra ID or Google Workspace. Verify your domain, connect the identity provider in the WorkOS Admin Portal, then optionally require SSO so members reach the team only after signing in through it. The owner and SSO-exempt members are the break-glass accounts.", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error { return c.Help() }}
	status := &cobra.Command{Use: "status <team>", Short: "Show SSO connection, verified domains, Require SSO and exemptions", Long: "Show the team's single sign-on state as last read from WorkOS: the identity provider connection and its state, verified and pending domains, whether Require SSO is on and its SSO session length, JIT provisioning, and the members marked SSO exempt. Owners, admins and auditors can read it. Use --json for every field.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		return printTeamIdentity(c, args[0], func(client *api.Client) (api.TeamIdentity, error) { return client.TeamIdentity(c.Context(), args[0]) }, ssoSummary)
	}}
	status.Flags().Bool("json", false, "print JSON")
	setup := &cobra.Command{Use: "setup <team>", Short: "Open the WorkOS Admin Portal to verify a domain and connect your identity provider", Long: "Print, and open in a browser where possible, a short-lived WorkOS Admin Portal link for one setup step. Use --step domain to verify a domain with a DNS TXT record, then --step sso to connect the identity provider with the guide for your provider. The team needs enough seats; the first run creates its WorkOS organization.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		step, _ := c.Flags().GetString("step")
		intent := map[string]string{"domain": "domain_verification", "sso": "sso"}[step]
		if intent == "" {
			return localArgumentError("--step must be domain or sso")
		}
		return openTeamPortal(c, args[0], intent)
	}}
	setup.Flags().String("step", "sso", "domain (verify a domain) or sso (connect the identity provider)")
	enforce := &cobra.Command{Use: "enforce <team> <on|off>", Short: "Turn Require SSO on or off (shows who loses access first)", Long: "Turn Require SSO on or off. When on, members reach the team only after an SSO sign-in within the SSO session length, set with --session. Turning it on first lists the members who would lose access until they sign in through SSO and prints the exact command with --confirm to proceed. Open connections keep running.", Args: commandArgs(cobra.ExactArgs(2)), RunE: runSSOEnforce}
	enforce.Flags().String("session", "", "SSO session length, e.g. 24h or 7d (1–30 days)")
	enforce.Flags().String("confirm", "", "the review code printed when turning Require SSO on")
	enforce.Flags().Bool("json", false, "print JSON")
	exempt := &cobra.Command{Use: "exempt <team> <account>", Short: "Mark or unmark a member as SSO exempt (break-glass)", Long: "Mark a member as SSO exempt so they keep access to the team without SSO, for contractors and emergency admins, or remove the mark with --remove. The owner is always exempt. Only a few members may be exempt, and every access without SSO is recorded in the audit log once a day per person.", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(c *cobra.Command, args []string) error {
		remove, _ := c.Flags().GetBool("remove")
		return printTeamIdentity(c, args[0], func(client *api.Client) (api.TeamIdentity, error) {
			return client.SetTeamSSOExempt(c.Context(), args[0], args[1], !remove)
		}, ssoSummary)
	}}
	exempt.Flags().Bool("remove", false, "remove the exemption")
	exempt.Flags().Bool("json", false, "print JSON")
	root.AddCommand(status, setup, enforce, exempt)
	return root
}

func runSSOEnforce(c *cobra.Command, args []string) error {
	if args[1] != "on" && args[1] != "off" {
		return localArgumentError("say on or off")
	}
	on := args[1] == "on"
	in := api.TeamSSOSettings{Required: &on}
	if v, _ := c.Flags().GetString("session"); v != "" {
		at, err := activityTime(v, time.Unix(0, 0))
		if err != nil {
			return localArgumentError("--session takes a length such as 24h or 7d")
		}
		seconds := int(-at.Unix())
		in.SessionSeconds = &seconds
	}
	in.Confirm, _ = c.Flags().GetString("confirm")
	err := printTeamIdentity(c, args[0], func(client *api.Client) (api.TeamIdentity, error) {
		return client.UpdateTeamSSO(c.Context(), args[0], in)
	}, ssoSummary)
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "sso_review_required" {
		return err
	}
	digest, _ := apiErr.Details["digest"].(string)
	members, _ := apiErr.Details["members"].([]any)
	out := c.OutOrStdout()
	fmt.Fprintf(out, "Requiring SSO locks out %d member(s) until they sign in through your identity provider:\n", len(members))
	for _, m := range members {
		if member, ok := m.(map[string]any); ok {
			fmt.Fprintf(out, "  %v (%v)\n", member["email"], member["account_id"])
		}
	}
	fmt.Fprintf(out, "Nothing has changed. To confirm, run:\npb team sso enforce %s on --confirm %s\n", args[0], digest)
	return exitCodeError{code: 2}
}

func ssoSummary(w io.Writer, s api.TeamIdentity) error {
	fmt.Fprintf(w, "Seats: %d (SSO from %d)\n", s.Seats, s.MinSeats)
	if len(s.SSO.Connections) == 0 {
		fmt.Fprintln(w, "Identity provider: not connected (pb team sso setup <team>)")
	}
	for _, conn := range s.SSO.Connections {
		fmt.Fprintf(w, "Identity provider: %s (%s)\n", conn.Type, conn.State)
	}
	for _, d := range s.SSO.Domains {
		fmt.Fprintf(w, "Domain: %s (%s)\n", d.Domain, d.State)
	}
	required := "off"
	if s.SSO.Required {
		required = fmt.Sprintf("on, SSO session %s", time.Duration(s.SSO.SessionSeconds)*time.Second)
	}
	fmt.Fprintf(w, "Require SSO: %s\nJIT provisioning: %t\nSSO exempt: %s\n", required, s.SSO.JIT, strings.Join(s.SSO.Exempt, ", "))
	return nil
}

func teamSCIMCommand() *cobra.Command {
	root := &cobra.Command{Use: "scim", Short: "Directory sync (SCIM) for a team (owners and admins)", Long: "Directory sync for a team through WorkOS: connect your company directory (Okta, Entra ID, Google Workspace and others) and Paperboat adds, updates and removes members to match it within seconds. Map directory groups to team roles. Removing someone in the directory removes them from the team at once and ends their open sessions.", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error { return c.Help() }}
	status := &cobra.Command{Use: "status <team>", Short: "Show directory sync state, last sync and group roles", Long: "Show the team's directory sync state: the connected directory and its state, the number of directory users, when it last synced and any problem, whether only mapped groups are provisioned, and every directory group with the team role it maps to. Owners, admins and auditors can read it. Use --json for every field.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		return printTeamIdentity(c, args[0], func(client *api.Client) (api.TeamIdentity, error) { return client.TeamIdentity(c.Context(), args[0]) }, scimSummary)
	}}
	status.Flags().Bool("json", false, "print JSON")
	setup := &cobra.Command{Use: "setup <team>", Short: "Open the WorkOS Admin Portal to connect your directory", Long: "Print, and open in a browser where possible, a short-lived WorkOS Admin Portal link to connect your company directory with SCIM. Your IT admin follows the guide for your directory there. Once connected, Paperboat provisions members within seconds; map groups to roles with pb team scim map. The team needs enough seats.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		return openTeamPortal(c, args[0], "dsync")
	}}
	mapCmd := &cobra.Command{Use: "map <team> <group> <admin|billing|auditor|member|none>", Short: "Map a directory group to a team role", Long: "Map a directory group, by name or ID, to a team role: admin, billing, auditor or member, or none to remove the mapping. Someone in several mapped groups gets the highest role. With --mapped-only, only members of mapped groups are provisioned and everyone else is removed. Members managed by the directory change only through it.", Args: commandArgs(cobra.ExactArgs(3)), RunE: func(c *cobra.Command, args []string) error {
		role := args[2]
		if role == "none" {
			role = ""
		} else if role != "admin" && role != "billing" && role != "auditor" && role != "member" {
			return localArgumentError("role must be admin, billing, auditor, member or none")
		}
		return printTeamIdentity(c, args[0], func(client *api.Client) (api.TeamIdentity, error) {
			current, err := client.TeamIdentity(c.Context(), args[0])
			if err != nil {
				return current, err
			}
			group := ""
			for _, g := range current.Directory.Groups {
				if g.ID == args[1] || strings.EqualFold(g.Name, args[1]) {
					group = g.ID
				}
			}
			if group == "" {
				return current, localArgumentError("no directory group named " + args[1] + "; run pb team scim status " + args[0])
			}
			in := api.TeamDirectorySettings{GroupRoles: map[string]string{group: role}}
			if c.Flags().Changed("mapped-only") {
				only, _ := c.Flags().GetBool("mapped-only")
				in.MappedOnly = &only
			}
			return client.UpdateTeamDirectory(c.Context(), args[0], in)
		}, scimSummary)
	}}
	mapCmd.Flags().Bool("mapped-only", false, "provision only members of mapped groups")
	mapCmd.Flags().Bool("json", false, "print JSON")
	root.AddCommand(status, setup, mapCmd)
	return root
}

func scimSummary(w io.Writer, s api.TeamIdentity) error {
	d := s.Directory
	if d.Directory == nil {
		fmt.Fprintln(w, "Directory: not connected (pb team scim setup <team>)")
		return nil
	}
	fmt.Fprintf(w, "Directory: %s (%s), %d users\n", d.Directory.Type, d.Directory.State, d.Users)
	if d.SyncedAt != nil {
		fmt.Fprintf(w, "Last sync: %s\n", d.SyncedAt.Format(time.RFC3339))
	}
	if d.Error != "" {
		fmt.Fprintf(w, "Problem: %s\n", d.Error)
	}
	fmt.Fprintf(w, "Only mapped groups: %t\n", d.MappedOnly)
	for _, g := range d.Groups {
		role := g.Role
		if role == "" {
			role = "(not mapped)"
		}
		fmt.Fprintf(w, "Group %s: %s\n", g.Name, role)
	}
	return nil
}

func printTeamIdentity(c *cobra.Command, team string, call func(*api.Client) (api.TeamIdentity, error), summary func(io.Writer, api.TeamIdentity) error) error {
	if !validTeamCLIIdentifier(team) {
		return localArgumentError("use an exact team ID")
	}
	client, err := backendForCommand(c)
	if err != nil {
		return err
	}
	out, err := call(client)
	if err != nil {
		return err
	}
	if jsonOutput, _ := c.Flags().GetBool("json"); jsonOutput {
		return json.NewEncoder(c.OutOrStdout()).Encode(out)
	}
	return summary(c.OutOrStdout(), out)
}

func openTeamPortal(c *cobra.Command, team, intent string) error {
	if !validTeamCLIIdentifier(team) {
		return localArgumentError("use an exact team ID")
	}
	client, err := backendForCommand(c)
	if err != nil {
		return err
	}
	link, err := client.TeamIdentityPortal(c.Context(), team, intent)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.OutOrStdout(), "Open this link within 5 minutes to continue setup in the WorkOS Admin Portal:\n%s\n", link)
	_ = openBrowser(link)
	return nil
}
