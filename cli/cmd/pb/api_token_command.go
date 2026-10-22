package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
)

func apiTokenCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "api-token", Short: "Create, list and revoke workspace API tokens", Long: `An API token lets a script or service call the Paperboat API as you, in one
workspace, until it expires or is revoked. A scope narrows what it may do using
the policy's names; a scope never gives more access than you have. The secret
is shown once, when the token is created.`, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	root.AddCommand(apiTokenCreateCommand(), apiTokenListCommand(), apiTokenRevokeCommand())
	return root
}

// parseExpiryDays reads "90d" or "90" as a number of days.
func parseExpiryDays(value string) (int, error) {
	days, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(value), "d"))
	if err != nil || days < 1 {
		return 0, invocationError(fmt.Errorf("--expires %q is not a number of days such as 90d", value))
	}
	return days, nil
}

// readTokenScope reads --scope or --scope-file and checks it is a list of
// {dst, allow, ports?} entries; the server validates the names.
func readTokenScope(command *cobra.Command) (json.RawMessage, error) {
	inline, _ := command.Flags().GetString("scope")
	file, _ := command.Flags().GetString("scope-file")
	if inline != "" && file != "" {
		return nil, invocationError(errors.New("use either --scope or --scope-file, not both"))
	}
	data := []byte(inline)
	if file != "" {
		var err error
		if data, err = os.ReadFile(file); err != nil {
			return nil, invocationError(fmt.Errorf("read the scope file: %w", err))
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var entries []api.APITokenScope
	if err := decoder.Decode(&entries); err != nil || len(entries) == 0 {
		return nil, invocationError(errors.New(`the scope must be a JSON list of {"dst": [...], "allow": [...], "ports"?: [...]} entries`))
	}
	return json.RawMessage(data), nil
}

func apiTokenCreateCommand() *cobra.Command {
	command := &cobra.Command{Use: "create <name>", Short: "Create an API token and print its secret once", Long: `Create an API token in the active workspace that acts as you until it
expires. --expires sets its lifetime in days (default 90d). --scope or
--scope-file narrows it to a JSON list of {"dst", "allow", "ports"} entries
using the policy's names. The secret is printed once; store it right away.`, Example: "  pb api-token create deploy\n  pb api-token create ci --expires 30d --scope '[{\"dst\":[\"tag:ci\"],\"allow\":[\"exec\"]}]'\n  pb api-token create ci --json", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		expires, _ := command.Flags().GetString("expires")
		days, err := parseExpiryDays(expires)
		if err != nil {
			return err
		}
		scope, err := readTokenScope(command)
		if err != nil {
			return err
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		created, err := client.CreateAPIToken(command.Context(), workspace, api.APITokenCreate{Name: args[0], ExpiresInDays: days, Scope: scope})
		if details, ok := api.PolicyDetails(err, "invalid_api_token_scope"); ok {
			printPolicyErrors(command.ErrOrStderr(), details.Errors)
		}
		if err != nil {
			return err
		}
		if wantsJSON(command) {
			return writeJSON(command.OutOrStdout(), created)
		}
		token := created.APIToken
		fmt.Fprintf(command.ErrOrStderr(), "Created API token %s (%s) in %s, expires %s.\nThe secret below is shown once and cannot be shown again. Store it now:\n", token.Name, token.ID, token.Workspace, token.ExpiresAt.Local().Format("2006-01-02"))
		_, err = fmt.Fprintln(command.OutOrStdout(), created.Secret)
		return err
	}}
	command.Flags().String("expires", "90d", "lifetime in days, such as 30d")
	command.Flags().String("scope", "", "JSON list of {dst, allow, ports} entries that narrow the token")
	command.Flags().String("scope-file", "", "read the scope from a JSON file")
	command.Flags().Bool("json", false, "print JSON, including the secret")
	return command
}

func apiTokenState(token api.APIToken, now time.Time) string {
	switch {
	case token.RevokedAt != nil:
		return "revoked"
	case !token.ExpiresAt.After(now):
		return "expired"
	}
	return "active"
}

func apiTokenListCommand() *cobra.Command {
	command := &cobra.Command{Use: "list", Short: "List API tokens", Long: `List the API tokens of the active workspace: your own, or every member's when
you are an owner or admin. The table shows each token's name, id, creator,
expiry, last use, state and whether a scope narrows it. Secrets are never
shown again after creation.`, Example: "  pb api-token list\n  pb api-token list --json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		tokens, err := client.APITokens(command.Context(), workspace)
		if err != nil {
			return err
		}
		if wantsJSON(command) {
			return writeJSON(command.OutOrStdout(), map[string]any{"api_tokens": tokens})
		}
		return printAPITokens(command.OutOrStdout(), tokens)
	}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func printAPITokens(w io.Writer, tokens []api.APIToken) error {
	if len(tokens) == 0 {
		_, err := fmt.Fprintln(w, "No API tokens. Create one with pb api-token create <name>.")
		return err
	}
	now := time.Now()
	writer := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tID\tCREATED BY\tEXPIRES\tLAST USED\tSTATE\tSCOPED")
	for _, token := range tokens {
		lastUsed, scoped := "-", "no"
		if token.LastUsedAt != nil {
			lastUsed = token.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		if len(token.Scope) > 0 {
			scoped = "yes"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", token.Name, token.ID, token.CreatedBy, token.ExpiresAt.Local().Format("2006-01-02"), lastUsed, apiTokenState(token, now), scoped)
	}
	return writer.Flush()
}

// findAPIToken matches an id exactly, or else a name among tokens not yet revoked.
func findAPIToken(tokens []api.APIToken, ref string) (api.APIToken, error) {
	var matches []api.APIToken
	for _, token := range tokens {
		if token.ID == ref {
			return token, nil
		}
		if token.Name == ref && token.RevokedAt == nil {
			matches = append(matches, token)
		}
	}
	switch len(matches) {
	case 0:
		return api.APIToken{}, localArgumentError("no active API token has that name or id; pb api-token list shows them")
	case 1:
		return matches[0], nil
	}
	return api.APIToken{}, localArgumentError("several active API tokens have that name; revoke one by its id from pb api-token list")
}

func apiTokenRevokeCommand() *cobra.Command {
	command := &cobra.Command{Use: "revoke <name|id>", Short: "Revoke an API token", Long: `Revoke an API token by name or id. Requests that use it fail from then on.
You can revoke your own tokens; owners and admins can revoke any member's.
A name shared by several members' tokens must be given as an id. Without
--yes, the command asks for confirmation first.`, Example: "  pb api-token revoke deploy\n  pb api-token revoke apitok_123 --yes", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		tokens, err := client.APITokens(command.Context(), workspace)
		if err != nil {
			return err
		}
		token, err := findAPIToken(tokens, args[0])
		if err != nil {
			return err
		}
		if yes, _ := command.Flags().GetBool("yes"); !yes {
			confirmed, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Revoke API token " + token.Name + "?", Description: "Scripts and services that use this token stop working at once. This cannot be undone.", Stdin: os.Stdin, Output: command.ErrOrStderr()})
			if err != nil {
				return invocationError(fmt.Errorf("confirm the revoke: %w; run again with --yes", err))
			}
			if !confirmed {
				return errors.New("not revoked")
			}
		}
		revoked, err := client.RevokeAPIToken(command.Context(), workspace, token.ID)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Revoked API token %s (%s).\n", revoked.Name, revoked.ID)
		return err
	}}
	command.Flags().Bool("yes", false, "revoke without asking")
	return command
}
