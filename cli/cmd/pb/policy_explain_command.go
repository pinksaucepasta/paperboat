package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func policyExplainCommand() *cobra.Command {
	command := &cobra.Command{Use: "explain", Short: "Explain who can do what under the policy", Long: `Explain an access decision. With --src, --dst and --action, print whether it
is allowed, the reason and the rules that decided it. With --dst and --action
only, list who can do it; with --src only, list what that user can do. --file
explains a draft instead of the saved policy and --api-token narrows the answer
to one of your API tokens. Members can explain their own access; owners and
admins anyone's.`, Example: "  pb policy explain --src user:ana@example.test --dst machine:db --action private_access --port 5432\n  pb policy explain --dst machine:db --action exec\n  pb policy explain --src user:ana@example.test --file draft.hujson", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		var request api.PolicyExplainRequest
		request.Src, _ = command.Flags().GetString("src")
		request.Dst, _ = command.Flags().GetString("dst")
		request.Action, _ = command.Flags().GetString("action")
		request.Port, _ = command.Flags().GetInt("port")
		request.APIToken, _ = command.Flags().GetString("api-token")
		if request.Src == "" && request.Dst == "" {
			return invocationError(fmt.Errorf("give --src, or --dst with --action"))
		}
		if request.Dst != "" && request.Action == "" {
			return invocationError(fmt.Errorf("--dst needs --action"))
		}
		if file, _ := command.Flags().GetString("file"); file != "" {
			text, err := readPolicyFile(command, file)
			if err != nil {
				return err
			}
			request.Policy = text
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		explanation, err := client.ExplainPolicy(command.Context(), workspace, request)
		if err != nil {
			return err
		}
		if wantsJSON(command) {
			return writeJSON(command.OutOrStdout(), explanation)
		}
		return printExplanation(command.OutOrStdout(), explanation)
	}}
	command.Flags().String("src", "", "who: user:<email>")
	command.Flags().String("dst", "", "what: machine:<alias>, preview:<name>, tunnel:<name>, tag:<tag>")
	command.Flags().String("action", "", "the action, such as exec or private_access")
	command.Flags().Int("port", 0, "the port, for private_access")
	command.Flags().String("file", "", "explain a draft policy file instead of the saved policy")
	command.Flags().String("api-token", "", "narrow the answer to one of your API tokens, by name")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func printExplanation(w io.Writer, explanation api.PolicyExplanation) error {
	if decision := explanation.Decision; decision != nil {
		verdict := "DENIED"
		if decision.Allowed {
			verdict = "ALLOWED"
		}
		fmt.Fprintf(w, "%s: %s\n", verdict, decision.Reason)
		if len(decision.Rules) > 0 {
			fmt.Fprintf(w, "Rules: %s\n", strings.Join(decision.Rules, ", "))
		}
		if decision.Guardrail != "" {
			fmt.Fprintf(w, "Guardrail: %s\n", decision.Guardrail)
		}
	}
	for _, table := range []struct {
		title string
		rows  []api.PolicyAccess
	}{{"Who can", explanation.WhoCan}, {"What can", explanation.WhatCan}} {
		if table.rows == nil {
			continue
		}
		fmt.Fprintf(w, "%s (%d):\n", table.title, len(table.rows))
		writer := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "SUBJECT\tTARGET\tACTION\tPORTS\tREASON\tRULES")
		for _, row := range table.rows {
			ports, rules := formatPorts(row.Ports), strings.Join(row.Rules, ",")
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Subject, row.Target, row.Action, dashIfEmpty(ports), row.Reason, dashIfEmpty(rules))
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
