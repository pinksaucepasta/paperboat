package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/shared/machinename"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

func userMachineCobraCommand() *cobra.Command {
	machine := &cobra.Command{Use: "machine", Short: "Manage machines", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return actionHomeMachines(command)
		}
		return command.Help()
	}}
	add := machineEnrollmentCommand()
	list := machineListCommand()
	rename := machineRenameCommand()
	revoke := machineRevokeCommand()
	availability := machineAvailabilityCommand()
	capabilities := machineCapabilitiesCommand()
	machine.AddCommand(add, list, rename, revoke, availability, capabilities, machineTagCommand())
	return machine
}

func machineListCommand() *cobra.Command {
	list := &cobra.Command{Use: "list", Short: "List enrolled machines", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		maxItems, err := listPagingMax(command)
		if err != nil {
			return err
		}
		// A tag filter is applied here, so it reads up to the --all bound and reports the matches.
		tags, _ := command.Flags().GetStringArray("tag")
		read := maxItems
		if len(tags) > 0 {
			read = maxListAll
		}
		machines, total, err := client.ListUserMachinesPage(ctx.Context, inventoryFilters(command), read)
		if err != nil {
			return friendlyCommandError(err)
		}
		if len(tags) > 0 {
			if machines, err = machineTagFilter(command, machines); err != nil {
				return err
			}
			total = len(machines)
			if len(machines) > maxItems {
				machines = machines[:maxItems]
			}
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machines": machines, "total": total})
		}
		defer writeListFooter(command.ErrOrStderr(), len(machines), total, "machines")
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tOWNERSHIP\tSTATE\tPERMISSIONS\tTAGS\tID")
		for _, item := range machines {
			state := item.State
			if item.Online {
				state = "online"
			}
			ownership := item.Ownership
			if item.OwnerTeamID != "" {
				ownership = "team:" + item.OwnerTeamID
			}
			permissions := strings.Join(item.Permissions, ",")
			if item.CanManage {
				permissions = strings.Trim(permissions+",manage", ",")
			}
			if permissions == "" {
				permissions = "none"
			}
			tags := strings.Join(item.Tags, ",")
			if tags == "" {
				tags = "-"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", item.Alias, ownership, state, permissions, tags, item.ID)
		}
		return writer.Flush()
	}}
	list.Flags().Bool("json", false, "print JSON")
	list.Flags().StringArray("tag", nil, "only machines with this tag (repeat for several)")
	inventoryFilterFlags(list)
	addListPaging(list)
	return list
}

func machineRenameCommand() *cobra.Command {
	rename := &cobra.Command{Use: "rename <machine> <name>", Short: "Rename a machine", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
		newName := strings.TrimSpace(args[1])
		if err := machinename.Validate(newName); err != nil {
			return invocationError(fmt.Errorf("invalid machine name: %w", err))
		}
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machine, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if !jsonOutput {
			fmt.Fprintf(command.ErrOrStderr(), "Machine: %s (%s)\n", machine.Alias, machine.ID)
		}
		updated, err := client.RenameUserMachine(ctx.Context, machine.ID, newName)
		if err != nil {
			return friendlyCommandError(err)
		}
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": updated, "outcome": "renamed"})
		}
		fmt.Fprintf(command.OutOrStdout(), "Renamed machine %s to %s (%s).\n", machine.Alias, updated.Alias, updated.ID)
		return nil
	}}
	rename.Flags().Bool("json", false, "print JSON")
	return rename
}

func machineRevokeCommand() *cobra.Command {
	revoke := &cobra.Command{Use: "revoke <machine>", Short: "Disconnect and revoke a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(cobraCommand *cobra.Command, args []string) error {
		ctx := actionContext(cobraCommand, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		jsonOutput, _ := cobraCommand.Flags().GetBool("json")
		target, err := resolveUserMachine(ctx.Context, client, args[0])
		if errors.Is(err, resolver.ErrMachineNotFound) {
			// A disconnected machine is no longer listed by default, yet revoking it again is done, not a
			// failure (laneo-ha/hb, round 26 reported operation_failed/local_io).
			if machine, found := findDisconnectedMachine(ctx.Context, client, args[0]); found {
				return reportMachineRevoked(cobraCommand, jsonOutput, machine.ID, machine.Alias, "already_disconnected")
			}
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		userMachineID, alias := target.ID, target.Alias
		if err := confirmMutation(cobraCommand, "machine-revoke:"+userMachineID, fmt.Sprintf("Revoke machine %s (%s)? Its enrollment and credentials will lose authority.", alias, userMachineID)); err != nil {
			return err
		}
		if err := client.DisconnectUserMachine(ctx.Context, userMachineID); err != nil {
			return friendlyCommandError(err)
		}
		return reportMachineRevoked(cobraCommand, jsonOutput, userMachineID, alias, "confirmed")
	}}
	revoke.Flags().String("confirm", "", "six-character confirmation code from the preview")
	revoke.Flags().Bool("json", false, "print JSON")
	return revoke
}

func machineAvailabilityCommand() *cobra.Command {
	availability := &cobra.Command{Use: "availability <machine>", Short: "Set machine sleep availability", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		modeFlag, _ := command.Flags().GetString("mode")
		mode := strings.ReplaceAll(strings.TrimSpace(modeFlag), "-", "_")
		if mode != "allow_sleep" && mode != "keep_awake" {
			return localArgumentError("availability --mode must be allow-sleep or keep-awake")
		}
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machine, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		if mode == "keep_awake" {
			if err := confirmMutation(command, fmt.Sprintf("machine-keep-awake:%s:%d", machine.ID, machine.Availability.DesiredVersion), fmt.Sprintf("Keep machine %s (%s) available while idle? This can increase battery use and heat and keep a closed-lid machine awake.", machine.Alias, machine.ID)); err != nil {
				return err
			}
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if !jsonOutput {
			fmt.Fprintf(command.ErrOrStderr(), "Machine: %s (%s)\n", machine.Alias, machine.ID)
		}
		policy, err := client.SetUserMachineAvailability(ctx.Context, machine.ID, mode, newIdempotencyKey(), machine.Availability.DesiredVersion)
		if err != nil {
			return friendlyCommandError(err)
		}
		policy = waitForAvailabilityObservation(ctx.Context, client, machine.ID, policy, 5*time.Second)
		outcome := "pending"
		if policy.Status == "applied" && policy.ObservedVersion == policy.DesiredVersion && policy.ObservedMode == policy.DesiredMode {
			outcome = "applied"
		}
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": map[string]string{"id": machine.ID, "alias": machine.Alias}, "availability": policy, "outcome": outcome, "retry": "automatic"})
		}
		if outcome == "applied" {
			fmt.Fprintf(command.OutOrStdout(), "Availability %s applied to %s.\n", strings.ReplaceAll(mode, "_", "-"), machine.Alias)
		} else {
			fmt.Fprintf(command.OutOrStdout(), "Availability %s saved for %s; application is durably %s.\n", strings.ReplaceAll(mode, "_", "-"), machine.Alias, policy.Status)
		}
		return nil
	}}
	availability.Flags().String("mode", "", "availability mode: allow-sleep or keep-awake")
	availability.Flags().String("confirm", "", "six-character confirmation code for keep-awake mode")
	availability.Flags().Bool("json", false, "print JSON")
	return availability
}

func machineCapabilitiesCommand() *cobra.Command {
	capabilities := &cobra.Command{Use: "capabilities <machine>", Short: "Set incoming services for a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		overrides := make(map[string]bool, 4)
		for _, name := range []string{"terminal", "managed-ssh", "file-receive", "preview-tunnel"} {
			if command.Flags().Changed(name) {
				value, err := command.Flags().GetBool(name)
				if err != nil {
					return invocationError(err)
				}
				overrides[name] = value
			}
		}
		if len(overrides) == 0 {
			return localArgumentError("set at least one capability flag")
		}
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machineValue, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		desired := machineValue.MachineCapabilities.Desired
		for name, destination := range map[string]*bool{"terminal": &desired.Terminal, "managed-ssh": &desired.ManagedSSH, "file-receive": &desired.FileReceive, "preview-tunnel": &desired.PreviewTunnel} {
			if value, changed := overrides[name]; changed {
				*destination = value
			}
		}
		policy, err := client.SetUserMachineCapabilities(ctx.Context, machineValue.ID, newIdempotencyKey(), desired, machineValue.MachineCapabilities.DesiredVersion)
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": map[string]string{"id": machineValue.ID, "alias": machineValue.Alias}, "machine_capabilities": policy})
		}
		fmt.Fprintf(command.OutOrStdout(), "Incoming capabilities saved for %s; application is %s.\n", machineValue.Alias, policy.Status)
		return nil
	}}
	capabilities.Flags().Bool("terminal", false, "accept Paperboat terminal and exec")
	capabilities.Flags().Bool("managed-ssh", false, "accept managed SSH/SCP/SFTP/rsync")
	capabilities.Flags().Bool("file-receive", false, "accept native Inbox transfers")
	capabilities.Flags().Bool("preview-tunnel", false, "serve preview and tunnel routes")
	capabilities.Flags().Bool("json", false, "print JSON")
	return capabilities
}

// findDisconnectedMachine looks requested up, by exact ID or name, among the machines already disconnected.
func findDisconnectedMachine(ctx context.Context, client *api.Client, requested string) (api.UserMachine, bool) {
	machines, err := client.ListUserMachinesFiltered(ctx, url.Values{"q": {requested}, "state": {"disconnected"}})
	if err != nil {
		return api.UserMachine{}, false
	}
	machine, err := resolveUserMachineIn(machines, requested)
	return machine, err == nil
}

func reportMachineRevoked(command *cobra.Command, jsonOutput bool, id, alias, outcome string) error {
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": map[string]string{"id": id, "alias": alias, "state": "disconnected"}, "outcome": outcome, "retry": "not_required"})
	}
	if outcome == "already_disconnected" {
		fmt.Fprintf(command.OutOrStdout(), "Machine %s (%s) is already disconnected.\n", alias, id)
		return nil
	}
	fmt.Fprintf(command.OutOrStdout(), "Disconnected machine %s (%s).\nIf this device was lost or compromised, rotate ENV keys.\n", alias, id)
	return nil
}
