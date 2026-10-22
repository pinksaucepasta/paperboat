package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

func policyCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "policy", Short: "Read, test and change the workspace access policy", Long: `The workspace policy is one file that says who may reach which machines,
previews and tunnels, and how. Read it with get, check a draft with test, see
what a change would do with set --dry-run, save it with set, explain any
decision with explain, and go back to an earlier version with restore. Every
save that changes anyone's access shows the changes and asks for confirmation.`, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	root.AddCommand(policyGetCommand(), policySetCommand(), policyTestCommand(), policyExplainCommand(), policyVersionsCommand(), policyRestoreCommand(), policySchemaCommand())
	return root
}

func policyClient(command *cobra.Command, args []string) (*api.Client, string, error) {
	workspace, err := effectiveWorkspace(command)
	if err != nil {
		return nil, "", err
	}
	client, err := backendClient(actionContext(command, args))
	return client, workspace, err
}

func wantsJSON(command *cobra.Command) bool {
	value, _ := command.Flags().GetBool("json")
	return value
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func policyGetCommand() *cobra.Command {
	command := &cobra.Command{Use: "get", Short: "Print the saved workspace policy", Long: `Print the saved policy file of the active workspace exactly as it was saved,
comments included, so it can be edited and saved again with pb policy set.
Use --version to print an earlier version; pb policy versions lists them. With
--json, print the whole policy record: version, digest, author and the names
the policy may use.`, Example: "  pb policy get > policy.hujson\n  pb policy get --version 3\n  pb policy get --json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		version, _ := command.Flags().GetInt64("version")
		current, err := client.GetPolicy(command.Context(), workspace, version)
		if err != nil {
			return err
		}
		if wantsJSON(command) {
			return writeJSON(command.OutOrStdout(), current)
		}
		_, err = io.WriteString(command.OutOrStdout(), current.Policy)
		return err
	}}
	command.Flags().Int64("version", 0, "print this saved version instead of the current one")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// readPolicyFile reads a policy file ("-" is stdin) and checks it locally, so syntax and
// shape errors show up before anything is sent.
func readPolicyFile(command *cobra.Command, path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(command.InOrStdin(), 4<<20))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the policy file: %w", err)
	}
	if _, problems := policy.Parse(data); len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(command.ErrOrStderr(), problem.String())
		}
		return "", localArgumentError(fmt.Sprintf("the policy file has %d error(s); fix them and try again", len(problems)))
	}
	return string(data), nil
}

func printPolicyErrors(w io.Writer, problems []api.PolicyError) {
	for _, problem := range problems {
		fmt.Fprintln(w, policy.Error{Path: problem.Path, Code: problem.Code, Message: problem.Message}.String())
	}
}

func formatPorts(ports []any) string {
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprint(port))
	}
	return strings.Join(parts, ",")
}

func printPolicyChanges(w io.Writer, changes []api.PolicyChange) {
	if len(changes) == 0 {
		fmt.Fprintln(w, "No access changes.")
		return
	}
	fmt.Fprintln(w, "Access changes:")
	for _, change := range changes {
		sign := "+"
		if change.Change == "lost" {
			sign = "-"
		}
		line := fmt.Sprintf("%s %s %s %s %s", sign, change.Change, change.Subject, change.Target, change.Action)
		if len(change.Ports) > 0 {
			line += " ports " + formatPorts(change.Ports)
		}
		if len(change.Rules) > 0 {
			line += " (" + strings.Join(change.Rules, ", ") + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// printValidation prints a validation result and returns an error when the draft is not
// valid or a test failed.
func printValidation(w io.Writer, result api.PolicyValidation) error {
	printPolicyErrors(w, result.Errors)
	fmt.Fprintf(w, "Tests: %d passed, %d failed\n", result.Tests.Passed, result.Tests.Failed)
	printPolicyChanges(w, result.AccessChanges)
	switch {
	case !result.Valid:
		return localArgumentError(fmt.Sprintf("the policy is not valid: %d error(s)", len(result.Errors)))
	case result.Tests.Failed > 0:
		return fmt.Errorf("%d policy test(s) failed", result.Tests.Failed)
	}
	return nil
}

func policySetCommand() *cobra.Command {
	command := &cobra.Command{Use: "set <file|->", Short: "Validate and save a workspace policy file", Long: `Check a policy file locally, validate it on the server against the saved
policy, print any errors, test results and the access each change grants or
removes, then save it. A save that changes anyone's access asks for
confirmation; --yes confirms the printed changes without asking. --dry-run
stops after validation. Use - to read the file from standard input.`, Example: "  pb policy set policy.hujson\n  pb policy set policy.hujson --dry-run\n  pb policy get | pb policy set - --yes --message \"tidy\"", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		text, err := readPolicyFile(command, args[0])
		if err != nil {
			return err
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		current, err := client.GetPolicy(command.Context(), workspace, 0)
		if err != nil {
			return err
		}
		result, err := client.ValidatePolicy(command.Context(), workspace, text)
		if err != nil {
			return err
		}
		out := command.OutOrStdout()
		if err := printValidation(out, result); err != nil {
			return err
		}
		if dryRun, _ := command.Flags().GetBool("dry-run"); dryRun {
			return nil
		}
		message, _ := command.Flags().GetString("message")
		save := api.PolicySave{Policy: text, ExpectedVersion: current.Version, Message: message}
		return confirmPolicySave(command, result.AccessChanges, result.Confirm, func(confirm string) (api.Policy, error) {
			save.Confirm = confirm
			return client.SavePolicy(command.Context(), workspace, save)
		})
	}}
	command.Flags().String("message", "", "describe the change in the version history")
	command.Flags().Bool("yes", false, "confirm the printed access changes without asking")
	command.Flags().Bool("dry-run", false, "validate and print the changes without saving")
	return command
}

// confirmPolicySave asks for confirmation when changes are pending, saves, and handles
// the server's 412 (the change list differs from what was confirmed) by showing the
// server's list and asking once more.
func confirmPolicySave(command *cobra.Command, changes []api.PolicyChange, confirm string, save func(string) (api.Policy, error)) error {
	for attempt := 0; ; attempt++ {
		if len(changes) > 0 {
			if err := confirmAccessChanges(command); err != nil {
				return err
			}
		} else {
			confirm = ""
		}
		saved, err := save(confirm)
		if details, ok := api.PolicyDetails(err, "policy_changes_unconfirmed"); ok && attempt == 0 && details.Confirm != "" {
			printPolicyChanges(command.OutOrStdout(), details.AccessChanges)
			changes, confirm = details.AccessChanges, details.Confirm
			continue
		}
		if details, ok := api.PolicyDetails(err, "invalid_policy"); ok {
			printPolicyErrors(command.ErrOrStderr(), details.Errors)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Saved policy version %d (%s).\n", saved.Version, saved.Digest)
		return err
	}
}

func confirmAccessChanges(command *cobra.Command) error {
	if yes, _ := command.Flags().GetBool("yes"); yes {
		return nil
	}
	confirmed, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Save these access changes?", Description: "The printed changes take effect for everyone in the workspace as soon as the policy is saved.", Stdin: os.Stdin, Output: command.ErrOrStderr()})
	if err != nil {
		return invocationError(fmt.Errorf("confirm the access changes: %w; review them and run again with --yes", err))
	}
	if !confirmed {
		return errors.New("not saved: the access changes were not confirmed")
	}
	return nil
}

func policyTestCommand() *cobra.Command {
	return &cobra.Command{Use: "test [file]", Short: "Validate a policy file and run its tests", Long: `Validate a policy file on the server without saving it, run the tests it
contains and print errors, test results and the access changes against the
saved policy. Without a file, the saved policy is tested. The command exits
with status 0 only when the policy is valid and every test passes.`, Example: "  pb policy test\n  pb policy test policy.hujson", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		var text string
		if len(args) == 1 {
			var err error
			if text, err = readPolicyFile(command, args[0]); err != nil {
				return err
			}
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		if len(args) == 0 {
			current, err := client.GetPolicy(command.Context(), workspace, 0)
			if err != nil {
				return err
			}
			text = current.Policy
		}
		result, err := client.ValidatePolicy(command.Context(), workspace, text)
		if err != nil {
			return err
		}
		return printValidation(command.OutOrStdout(), result)
	}}
}

func policyRestoreCommand() *cobra.Command {
	command := &cobra.Command{Use: "restore <version>", Short: "Restore an earlier policy version", Long: `Save an earlier policy version as the new current version. The history keeps
every version, so a restore can itself be undone. When the restore changes
anyone's access, the changes are printed and must be confirmed; --yes confirms
them without asking. pb policy versions lists the versions.`, Example: "  pb policy restore 3\n  pb policy restore 3 --yes --message \"undo\"", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		version, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || version < 1 {
			return invocationError(fmt.Errorf("version %q is not a positive number; pb policy versions lists them", args[0]))
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		current, err := client.GetPolicy(command.Context(), workspace, 0)
		if err != nil {
			return err
		}
		message, _ := command.Flags().GetString("message")
		restore := api.PolicySave{Version: version, ExpectedVersion: current.Version, Message: message}
		return confirmPolicySave(command, nil, "", func(confirm string) (api.Policy, error) {
			restore.Confirm = confirm
			return client.RestorePolicy(command.Context(), workspace, restore)
		})
	}}
	command.Flags().String("message", "", "describe the restore in the version history")
	command.Flags().Bool("yes", false, "confirm the printed access changes without asking")
	return command
}

func policySchemaCommand() *cobra.Command {
	return &cobra.Command{Use: "schema", Short: "Print the policy file JSON Schema", Long: `Print the JSON Schema of the workspace policy file, for editors that check and
complete the file as you type. The schema is built into pb, so this command
works offline and needs no sign-in; it matches the schema the server serves.`, Example: "  pb policy schema > policy.schema.json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		_, err := command.OutOrStdout().Write(policy.Schema)
		return err
	}}
}

func policyVersionsCommand() *cobra.Command {
	command := &cobra.Command{Use: "versions", Short: "List saved policy versions", Long: `List every saved version of the workspace policy, newest first, with who
saved it, when, how (saved, restored, rewritten or migrated), how many access
grants it added and removed, and its message. Print one with pb policy get
--version N and go back to one with pb policy restore N.`, Example: "  pb policy versions\n  pb policy versions --json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		versions, err := client.PolicyVersions(command.Context(), workspace)
		if err != nil {
			return err
		}
		if wantsJSON(command) {
			return writeJSON(command.OutOrStdout(), map[string]any{"versions": versions})
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "VERSION\tKIND\tSAVED\tAUTHOR\tCHANGES\tMESSAGE")
		for _, v := range versions {
			author := v.Author
			if v.APIToken != "" {
				author += " (token " + v.APIToken + ")"
			}
			fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t+%d -%d\t%s\n", v.Version, v.Kind, v.CreatedAt.Local().Format("2006-01-02 15:04"), author, v.Gained, v.Lost, v.Message)
		}
		return writer.Flush()
	}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}
