package main

import (
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/spf13/cobra"
)

// portCobraCommand names machine ports for browser addresses. The names are stored on the server, so they open
// on every computer of everyone allowed the port; a name grants no access.
func portCobraCommand() *cobra.Command {
	port := &cobra.Command{Use: "port", Short: "Name machine ports and set a machine's front desk", Long: `Name machine ports for private browser addresses and choose where unnamed addresses of a
machine go. Names live on the server and work on every computer of everyone allowed
the port; they never grant access to a port.`}
	name := &cobra.Command{Use: "name", Short: "Name machine ports for browser addresses", Long: `Give a machine port a name: <name>.<machine>.<domain> then opens that port in a
browser on every computer of everyone allowed that port. A name never grants access.
--machine names one machine's port (machine managers). --all-machines names that port
on every machine of the active workspace (owners and admins); a machine name of the
same label wins. --top-level --machine makes <name>.<domain> open that machine's port
(owners and admins); a machine with that name wins, and pb port name list shows the
top-level name as shadowed. Numeric labels always mean the port.`}
	scopeFlags := func(c *cobra.Command) {
		c.Flags().String("machine", "", "the machine whose port is named")
		c.Flags().Bool("all-machines", false, "name this port on every machine of the workspace")
		c.Flags().Bool("top-level", false, "make <name>.<domain> open the port of --machine")
	}
	set := &cobra.Command{Use: "set <name> <port>", Short: "Name a machine port", Long: `Give a port a name, or move an existing name to another port. With --machine the
address is <name>.<machine>.<domain>; with --all-machines the name works on every machine
of the active workspace; with --top-level --machine the address is <name>.<domain>.`, Example: "  pb port name set app 3000 --machine homelab\n  pb port name set grafana 3000 --all-machines\n  pb port name set jellyfin 8096 --top-level --machine media",
		Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[1])
			if err != nil || number < 1 || number > 65535 {
				return localArgumentError("the port must be a number from 1 to 65535")
			}
			client, in, alias, err := portNameTarget(command, args)
			if err != nil {
				return err
			}
			in.Name, in.Port = args[0], number
			record, err := client.SetPortName(command.Context(), in)
			if err != nil {
				return friendlyCommandError(err)
			}
			if wantsJSON(command) {
				return writeJSON(command.OutOrStdout(), record)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s now opens port %d %s.\n", portNameAddress(record), record.Port, portNameWhere(record.Scope, alias))
			return err
		}}
	list := &cobra.Command{Use: "list", Short: "List the workspace's port names", Long: `List the port names of the active workspace: names of its machines' ports, its
all-machines names and its top-level names. A top-level name whose label is also a
machine's name is shown as shadowed: that address opens the machine.`, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, err := backendClient(actionContext(command, args))
		if err != nil {
			return err
		}
		records, err := client.ListPortNames(command.Context())
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeJSON(command.OutOrStdout(), records)
		}
		return printPortNames(command.OutOrStdout(), records)
	}}
	unset := &cobra.Command{Use: "unset <name>", Short: "Remove a port name", Long: `Remove one port name of the active workspace. Say which one with --machine (a name
of that machine's port), --all-machines or --top-level. The same people who may set a
name may remove it; the port itself and its access do not change.`, Example: "  pb port name unset app --machine homelab\n  pb port name unset grafana --all-machines\n  pb port name unset jellyfin --top-level",
		Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
			client, in, _, err := portNameTarget(command, args)
			if err != nil {
				return err
			}
			records, err := client.ListPortNames(command.Context())
			if err != nil {
				return friendlyCommandError(err)
			}
			for _, record := range records {
				// A top-level name is one per workspace, whatever machine it opens.
				if record.Scope == in.Scope && record.Name == args[0] && (in.Scope != api.PortNameScopeMachine || record.MachineID == in.MachineID) {
					if err := client.DeletePortName(command.Context(), record.ID); err != nil {
						return friendlyCommandError(err)
					}
					_, err = fmt.Fprintf(command.OutOrStdout(), "Removed %s.\n", portNameAddress(record))
					return err
				}
			}
			return localArgumentError(fmt.Sprintf("no port name %q here; pb port name list shows the names", args[0]))
		}}
	for _, c := range []*cobra.Command{set, unset} {
		scopeFlags(c)
	}
	set.Flags().Bool("json", false, "print JSON")
	list.Flags().Bool("json", false, "print JSON")
	name.AddCommand(set, list, unset)
	port.AddCommand(name, frontDeskCommand())
	return port
}

// portNameTarget reads the scope flags; set needs --machine with --top-level, unset does not.
func portNameTarget(command *cobra.Command, args []string) (*api.Client, api.PortNameSet, string, error) {
	machine, _ := command.Flags().GetString("machine")
	all, _ := command.Flags().GetBool("all-machines")
	top, _ := command.Flags().GetBool("top-level")
	in := api.PortNameSet{Scope: api.PortNameScopeMachine}
	switch {
	case all && (top || machine != ""):
		return nil, in, "", localArgumentError("--all-machines cannot be combined with --machine or --top-level")
	case all:
		in.Scope = api.PortNameScopeAllMachines
	case top:
		in.Scope = api.PortNameScopeTopLevel
		if machine == "" && command.Name() == "set" {
			return nil, in, "", localArgumentError("--top-level needs --machine: the machine whose port the name opens")
		}
	case machine == "":
		return nil, in, "", localArgumentError("say --machine <machine>, --all-machines or --top-level")
	}
	client, err := backendClient(actionContext(command, args))
	if err != nil || machine == "" {
		return client, in, "", err
	}
	resolved, err := resolveUserMachine(command.Context(), client, machine)
	if err != nil {
		return nil, in, "", friendlyCommandError(err)
	}
	in.MachineID = resolved.ID
	return client, in, resolved.Alias, nil
}

func portNameAddress(record api.PortNameRecord) string {
	switch record.Scope {
	case api.PortNameScopeTopLevel:
		return record.Name + ".<domain>"
	case api.PortNameScopeAllMachines:
		return record.Name + ".<machine>.<domain>"
	}
	return record.Name + "." + record.MachineAlias + ".<domain>"
}

func portNameWhere(scope, alias string) string {
	if scope == api.PortNameScopeAllMachines {
		return "on every machine of the workspace"
	}
	return "on " + alias
}

func printPortNames(w io.Writer, records []api.PortNameRecord) error {
	if len(records) == 0 {
		_, err := fmt.Fprintln(w, "No port names. Add one with pb port name set <name> <port> --machine <machine>.")
		return err
	}
	writer := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ADDRESS\tPORT\tSCOPE\tNOTE")
	for _, record := range records {
		note := ""
		if record.Shadowed {
			note = "shadowed: machine " + record.Name + " has this name and wins"
		}
		fmt.Fprintf(writer, "%s\t%d\t%s\t%s\n", portNameAddress(record), record.Port, record.Scope, note)
	}
	return writer.Flush()
}

// frontDeskCommand shows or sets where an unmatched <name>.<machine>.<domain> goes.
func frontDeskCommand() *cobra.Command {
	command := &cobra.Command{Use: "front-desk <machine> [<port> [http|https]]", Short: "Show or set where unnamed addresses of a machine go", Long: `A machine's front desk is the port where <name>.<machine>.<domain> goes when no
port has that name: a reverse proxy such as Coolify, Traefik or Caddy. The default is
port 80 over http. Machine managers set it; the scheme defaults to https for port 443.`,
		Example: "  pb port front-desk homelab\n  pb port front-desk homelab 443 https", Args: commandArgs(cobra.RangeArgs(1, 3)), RunE: func(command *cobra.Command, args []string) error {
			client, err := backendClient(actionContext(command, args))
			if err != nil {
				return err
			}
			machine, err := resolveUserMachine(command.Context(), client, args[0])
			if err != nil {
				return friendlyCommandError(err)
			}
			var desk api.FrontDesk
			if len(args) == 1 {
				desk, err = client.GetFrontDesk(command.Context(), machine.ID)
			} else {
				in, parseErr := parseFrontDesk(args[1:])
				if parseErr != nil {
					return parseErr
				}
				desk, err = client.SetFrontDesk(command.Context(), machine.ID, in)
			}
			if err != nil {
				return friendlyCommandError(err)
			}
			if wantsJSON(command) {
				return writeJSON(command.OutOrStdout(), desk)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Front desk of %s: %s on port %d.\n", machine.Alias, desk.Scheme, desk.Port)
			return err
		}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func parseFrontDesk(args []string) (api.FrontDesk, error) {
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return api.FrontDesk{}, localArgumentError("the port must be a number from 1 to 65535")
	}
	desk := api.FrontDesk{Port: port, Scheme: "http"}
	if port == 443 {
		desk.Scheme = "https"
	}
	if len(args) == 2 {
		if args[1] != "http" && args[1] != "https" {
			return api.FrontDesk{}, localArgumentError("the scheme is http or https")
		}
		desk.Scheme = args[1]
	}
	return desk, nil
}
