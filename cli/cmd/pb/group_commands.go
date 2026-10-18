package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// installGroupCommandHandlers makes every command group that only holds
// subcommands behave like `pb env`: an unknown word is an invalid-command error
// instead of silently printing help, and --json lists the group's commands.
func installGroupCommandHandlers(command *cobra.Command) {
	for _, child := range command.Commands() {
		installGroupCommandHandlers(child)
	}
	if command == command.Root() || !command.HasSubCommands() || command.Runnable() {
		return
	}
	command.RunE = func(group *cobra.Command, args []string) error {
		if len(args) > 0 {
			return localArgumentError(fmt.Sprintf("%q is not a %s command. Run `%s --help` to list the commands.", args[0], group.CommandPath(), group.CommandPath()))
		}
		if jsonOutputRequested(group) {
			if err := writeCLIJSON(group.OutOrStdout(), map[string]any{"command": group.CommandPath(), "commands": cliCommandCatalog(group)}); err != nil {
				return err
			}
			return nil
		}
		return group.Help()
	}
}
