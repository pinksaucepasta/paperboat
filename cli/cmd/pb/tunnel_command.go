package main

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

func tunnelCobraCommandV1() *cobra.Command {
	root := &cobra.Command{Use: "tunnel [--ephemeral] [<port|url|path>]", Short: "Manage durable tunnels or start an ephemeral tunnel", Args: func(command *cobra.Command, args []string) error {
		ephemeral, _ := command.Flags().GetBool("ephemeral")
		if ephemeral {
			return previewTargetArgs(command, args)
		}
		return cobra.NoArgs(command, args)
	}, RunE: func(command *cobra.Command, args []string) error {
		ephemeral, _ := command.Flags().GetBool("ephemeral")
		if !ephemeral {
			if jsonOutputRequested(command) {
				return invocationError(errors.New("tunnel --json requires a subcommand or --ephemeral target"))
			}
			if previewInteractiveTerminal(command) {
				return actionHomeTunnelList(command)
			}
			return command.Help()
		}
		return runPreviewCobra(command, args)
	}, SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("ephemeral", false, "use the temporary preview lifecycle")
	configureEphemeralLaunchFlags(root)
	root.AddCommand(tunnelCreateCommand(), tunnelListCommand(), tunnelShowCommand(), tunnelRenameCommand())
	root.AddCommand(tunnelStatusCommand(), tunnelDoctorCommand(), tunnelLogsCommand())
	root.AddCommand(tunnelInspectCommand(), tunnelReplayCommand())
	for _, action := range []string{"pause", "resume", "delete"} {
		root.AddCommand(tunnelStateCommand(action))
	}
	root.AddCommand(tunnelRouteCommand(), tunnelDomainCommand())
	root.AddCommand(tunnelPolicyCommand())
	stop := &cobra.Command{Use: "stop <preview>", Short: "Stop an ephemeral tunnel", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		if strings.HasPrefix(args[0], "tunnel_") {
			return localArgumentError("`pb tunnel stop` stops a preview; use `pb tunnel pause` or `pb tunnel delete` for a durable tunnel")
		}
		return runPreviewStopCobra(command, args)
	}, SilenceUsage: true, SilenceErrors: true}
	stop.Flags().Bool("json", false, "print the stopped canonical resource as JSON")
	root.AddCommand(stop)
	return root
}

func tunnelPageFlag(command *cobra.Command) {
	command.Flags().String("cursor", "", "continue a previous page")
	command.Flags().Int("limit", 100, "maximum results (1-200)")
}

func tunnelPageFlags(command *cobra.Command) (string, int, error) {
	cursor, _ := command.Flags().GetString("cursor")
	limit, _ := command.Flags().GetInt("limit")
	if limit < 1 || limit > 200 {
		return "", 0, errors.New("limit must be between 1 and 200")
	}
	if len(cursor) > 4096 || strings.ContainsAny(cursor, "\x00\r\n") {
		return "", 0, errors.New("cursor is invalid")
	}
	return cursor, limit, nil
}
