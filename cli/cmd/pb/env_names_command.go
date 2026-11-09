package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/spf13/cobra"
)

const envNamesAgentGuide = "How an AI agent finds and uses a secret:\n" +
	"  pb env names                       which ENV names commands here receive (never values)\n" +
	"  pb env run -- <command>            run a command with those values in its environment\n" +
	"  printf %s \"$SECRET\" | pb env set NAME --value-stdin     store a secret the user gives you\n" +
	"Never print, echo or write a value to a file or log: let the program that needs it read it from its environment.\n" +
	"`pb env run` gives a command the whole workspace set; there is no per-name scoping."

func addEnvironmentNamesCommand(root *cobra.Command) {
	names := &cobra.Command{
		Use:   "names",
		Short: "List the names of the ENV variables commands on this machine receive (never values)",
		Long: "Print the names of the ENV variables that `pb env run` and the shell hook put into commands on this machine " +
			"for the active workspace: your team's, your own and this machine's, merged the way they are delivered. " +
			"Values, their length and their hash are never printed, so a person or an AI agent can see which secret to use " +
			"(for example STRIPE_API_KEY) without seeing it. --json adds the layer each name comes from: team, personal " +
			"or machine.\n\nA workspace owner can turn this off (\"Commands on machines can list ENV names\" on the Environment page); " +
			"then this command says so and prints nothing.\n\n" + envNamesAgentGuide,
		Example: "  pb env names\n  pb env names --json\n  pb env names | grep -q '^STRIPE_API_KEY$' && pb env run -- ./charge.sh",
		Args:    commandArgs(cobra.NoArgs),
		RunE:    runEnvironmentNames,
	}
	names.Flags().Bool("json", false, "print the names and the layer each comes from as JSON")
	names.AddCommand(envNamesAccessCommand())
	root.AddCommand(names)
}

func envNamesAccessCommand() *cobra.Command {
	access := &cobra.Command{
		Use:   "access [on|off]",
		Short: "Show or set whether commands on machines can list ENV names in this workspace",
		Long: "Show, or with on|off set, \"Commands on machines can list ENV names\" for the active workspace (--workspace, " +
			"default personal). It is on by default. Off, `pb env names` on the workspace's machines is refused; " +
			"`pb env run` still gives commands their values, so this is a policy for the listing, not a lock. " +
			"For a team only an owner or admin may change it.",
		Example: "  pb env names access\n  pb env names access off\n  pb env names access on --workspace my-team",
		Args:    commandArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			workspace, err := effectiveWorkspace(command)
			if err != nil {
				return err
			}
			return runEnvironmentNamesAccess(command, workspace, args)
		},
	}
	access.Flags().Bool("json", false, "print the setting as JSON")
	return access
}

func runEnvironmentNamesAccess(command *cobra.Command, workspace string, args []string) error {
	if len(args) == 1 && args[0] != "on" && args[0] != "off" {
		return localArgumentError("use: pb env names access [on|off]")
	}
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	ctx := command.Context()
	team := workspace != "personal"
	var current api.Team
	listable := false
	if team {
		if current, err = client.GetTeam(ctx, workspace); err == nil {
			listable = current.ENVNamesListable
		}
	} else {
		listable, err = client.ENVNamesListable(ctx)
	}
	if err != nil {
		return err
	}
	if len(args) == 1 {
		on := args[0] == "on"
		if team {
			_, err = client.MutateTeam(ctx, workspace, api.TeamMutationRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: current.Generation, Action: "env_names", ENVNamesListable: &on})
		} else {
			err = client.SetENVNamesListable(ctx, on)
		}
		if err != nil {
			return err
		}
		listable = on
	}
	if jsonOutputRequested(command) {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"workspace_id": workspace, "names_listable": listable})
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Commands on machines can list ENV names in %s: %s.\n", workspace, map[bool]string{true: "on", false: "off"}[listable])
	return err
}

func runEnvironmentNames(command *cobra.Command, _ []string) error {
	record, err := currentEnvHookRecord(command, envNamesFailure)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(command.Context(), envRunBudget)
	defer cancel()
	return writeEnvironmentNames(command, record.WorkspaceID, loadEnvironmentNames(ctx, command, record))
}

func envNamesFailure(reason string) error {
	return &envCommandFailure{message: "Paperboat ENV names not listed: " + reason + "."}
}

func writeEnvironmentNames(command *cobra.Command, workspace string, result envHookResult) error {
	fail := envNamesFailure
	switch {
	case result.quiet:
		return fail("this machine belongs to another Paperboat account")
	case result.disabled:
		return &envCommandFailure{message: "Listing ENV names from commands on this machine is turned off for this workspace. " +
			"A workspace owner can turn on \"Commands on machines can list ENV names\" on the Environment page of the console; " +
			"`pb env run -- <command>` still gives a command its values."}
	case result.reason != "":
		return fail(result.reason)
	}
	if jsonOutputRequested(command) {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"workspace_id": workspace, "names": result.names})
	}
	for _, entry := range result.names {
		if _, err := fmt.Fprintln(command.OutOrStdout(), entry.Name); err != nil {
			return err
		}
	}
	if len(result.names) == 0 {
		_, err := fmt.Fprintln(command.ErrOrStderr(), "No ENV variables reach commands on this machine in this workspace. Set one with `pb env set NAME --value-stdin`.")
		return err
	}
	return nil
}

// withNamesHint points a person or agent who hit the owner's vault (locked, or never set up on this
// computer) at the listing that needs no vault password.
func withNamesHint(err error) error {
	var failure *envCommandFailure
	if errors.As(err, &failure) && (errors.Is(failure.cause, environmentmanager.ErrVaultLocked) || errors.Is(failure.cause, environmentmanager.ErrVaultNotSetUp)) {
		return envCommandError(failure.cause, failure.message+" To see which ENV names commands on this machine receive, run `pb env names`.")
	}
	return err
}
