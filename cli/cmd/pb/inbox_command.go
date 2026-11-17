package main

import (
	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/spf13/cobra"
)

// inboxCobraCommand declares `pb inbox`: where received files land and how team
// file requests are answered.
func inboxCobraCommand() *cobra.Command {
	inbox := &cobra.Command{Use: "inbox", Short: "Manage the Paperboat Inbox", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	child := func(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
		command := &cobra.Command{Use: use, Short: short, Args: commandArgs(args), RunE: run}
		command.Flags().Bool("json", false, "print JSON")
		inbox.AddCommand(command)
		return command
	}
	child("path", "Show this machine's Inbox directory", cobra.NoArgs, actionRun(inboxPathCommand))
	child("set <directory>", "Set this machine's Inbox directory", cobra.ExactArgs(1), actionRun(inboxSetCommand))
	child("reset", "Restore the default Inbox directory", cobra.NoArgs, actionRun(inboxResetCommand))
	requests := child("requests", "List team file requests", cobra.NoArgs, actionRun(inboxRequestsCommand))
	inventoryFilterFlags(requests)
	addListPaging(requests)
	for _, decision := range []struct{ name, short string }{
		{"approve", "Approve an exact team file request"},
		{"decline", "Decline an exact team file request"},
	} {
		decision := decision
		command := child(decision.name+" <request-id>", decision.short, cobra.ExactArgs(1), actionRun(func(c *command.Context) error { return inboxDecisionCommand(c, decision.name) }))
		command.Flags().Uint("generation", 0, "expected request generation (default: the request's current one)")
	}
	child("policy [manual|automatic]", "Show or update team file acceptance", cobra.MaximumNArgs(1), actionRun(inboxPolicyCommand)).Flags().Bool("receipt-email", false, "send receipt email (applies when setting acceptance; omitted turns it off)")
	return inbox
}
