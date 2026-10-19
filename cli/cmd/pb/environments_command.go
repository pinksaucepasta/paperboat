package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// environmentsCobraCommand declares `pb environments`: the interactive picker on a
// terminal, a table or JSON otherwise.
func environmentsCobraCommand() *cobra.Command {
	environments := &cobra.Command{Use: "environments", Short: "List enrolled machines available to this account", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		jsonOutput, _ := command.Flags().GetBool("json")
		if !jsonOutput && term.IsTerminal(int(os.Stdin.Fd())) {
			return actionEnvironmentsList(command)
		}
		return actionRun(environmentsList)(command, args)
	}}
	environments.Flags().Bool("json", false, "print JSON")
	inventoryFilterFlags(environments)
	return environments
}

func environmentsList(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	machines, err := client.ListUserMachinesFiltered(c.Context, commandInventoryFilters(c))
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"machines": machines})
	}
	w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tPLATFORM\tID")
	for _, machine := range machines {
		state := machine.State
		if machine.Online && state == "" {
			state = "online"
		}
		platform := strings.Trim(strings.Join([]string{machine.Platform, machine.Architecture}, "/"), "/")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", machine.Alias, state, platform, machine.ID)
	}
	return w.Flush()
}
