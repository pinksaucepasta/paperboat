package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// configCobraCommand declares `pb config` with its built-in and feature subcommands.
func configCobraCommand() *cobra.Command {
	group := &cobra.Command{Use: "config", Short: "Inspect the local CLI config", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return command.Help()
		}
		return actionHomeConfig(command)
	}}
	for _, child := range []struct {
		use, short string
		args       cobra.PositionalArgs
		run        command.Action
		flags      func(*cobra.Command)
	}{
		{"status [environment]", "Show configuration synchronization status", cobra.MaximumNArgs(1), configStatus, func(c *cobra.Command) { c.Flags().Bool("json", false, "print JSON") }},
		{"unassign <environment>", "Remove a config repository assignment", cobra.ExactArgs(1), configUnassign, func(c *cobra.Command) {
			c.Flags().Bool("json", false, "print JSON")
			c.Flags().String("confirm", "", "six-character confirmation code from the preview")
		}},
		{"approve <environment>", "Approve the currently reviewed pull revision", cobra.ExactArgs(1), configApproveRevision, jsonFlag},
		{"team-default-set <team> <repository>", "Set a team's default pull repository", cobra.ExactArgs(2), configTeamDefaultSet, jsonFlag},
		{"team-default-adopt <team>", "Adopt a team default using your provider access", cobra.ExactArgs(1), configTeamDefaultAdopt, jsonFlag},
		{"team-default-unadopt", "Stop inheriting a team configuration default", cobra.NoArgs, configTeamDefaultUnadopt, jsonFlag},
		{"set <key> <value>", "Set a local configuration value", cobra.ExactArgs(2), configSet, jsonFlag},
		{"unset server", "Remove a local configuration value", cobra.ExactArgs(1), configUnset, jsonFlag},
		{"path", "Print the config file path", cobra.NoArgs, configPath, jsonFlag},
		{"show", "Print the effective config", cobra.NoArgs, configShow, jsonFlag},
	} {
		entry := &cobra.Command{Use: child.use, Short: child.short, Args: commandArgs(child.args), RunE: actionRun(child.run)}
		child.flags(entry)
		group.AddCommand(entry)
	}
	group.AddCommand(statusBarConfigCommand(), localAccessConfigCommand(), configConflictCobraCommand(), configForceCobraCommand(), customizationCommand(), configRepositoryCobraCommand(), configSyncCobraCommand())
	return group
}

// jsonFlag registers the unlabeled --json flag of the older config commands.
func jsonFlag(command *cobra.Command) { command.Flags().Bool("json", false, "") }

func configSet(c *command.Context) error {
	if c.Args().Len() != 2 {
		return localArgumentError("usage: pb config set server <url>")
	}
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return err
	}
	switch c.Args().First() {
	case "server":
		server, err := config.NormalizeServerURL(c.Args().Get(1))
		if err != nil {
			return usageError{err: err, publicMessage: "The server URL is invalid. Use an absolute HTTP or HTTPS URL without credentials, a query or a fragment."}
		}
		cfg.ServerURL = server
	case "ssh-target-port":
		port, parseErr := strconv.ParseUint(c.Args().Get(1), 10, 16)
		if parseErr != nil || port == 0 {
			return localArgumentError("ssh-target-port must be between 1 and 65535")
		}
		return configSetSSHTargetPort(c, uint16(port))
	default:
		return localArgumentError("usage: pb config set server <url> or pb config set ssh-target-port <1-65535>")
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"path": cfg.Path(), "key": c.Args().First(), "updated": true})
	}
	fmt.Fprintln(c.Writer, cfg.Path())
	return nil
}

func configUnset(c *command.Context) error {
	if c.Args().Len() != 1 || c.Args().First() != "server" {
		return localArgumentError("usage: pb config unset server")
	}
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return err
	}
	cfg.ServerURL = ""
	if err := cfg.Save(); err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"path": cfg.Path(), "key": "server", "unset": true})
	}
	fmt.Fprintln(c.Writer, cfg.Path())
	return nil
}

func configPath(c *command.Context) error {
	d, err := buildDeps(c)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"path": d.cfg.Path()})
	}
	fmt.Fprintln(c.Writer, d.cfg.Path())
	return nil
}

func configShow(c *command.Context) error {
	d, err := buildDeps(c)
	if err != nil {
		return err
	}
	sshPort, sshPortSet := configuredSSHTargetPort()
	if c.Bool("json") {
		shown := map[string]any{"path": d.cfg.Path(), "server_url": d.cfg.ServerURL, "auth_file_fallback": d.cfg.Auth.AllowFileFallback, "file_paste": d.cfg.FilePaste, "status_bar": d.cfg.StatusBar, "local_access": d.cfg.LocalAccess}
		if sshPortSet {
			shown["ssh_target_port"] = sshPort
		}
		return json.NewEncoder(c.Writer).Encode(shown)
	}
	fmt.Fprintf(c.Writer, "server_url: %s\n", orNone(d.cfg.ServerURL))
	if sshPortSet {
		fmt.Fprintf(c.Writer, "ssh_target_port: %d\n", sshPort)
	}
	fmt.Fprintf(c.Writer, "auth.file_fallback: %t\n", d.cfg.Auth.AllowFileFallback)
	fmt.Fprintf(c.Writer, "file_paste.max_queued_input_bytes: %d\n", d.cfg.FilePaste.MaxQueuedInputBytes)
	fmt.Fprintf(c.Writer, "status_bar.mode: %s\n", d.cfg.StatusBar.Mode)
	fmt.Fprintf(c.Writer, "status_bar.fullscreen: %s\n", d.cfg.StatusBar.Fullscreen)
	fmt.Fprintf(c.Writer, "status_bar.theme: %s\n", d.cfg.StatusBar.Theme)
	fmt.Fprintf(c.Writer, "local_access.domain: %s\n", orNone(effectiveLocalAccessDomain(c.Context, d.cfg)))
	fmt.Fprintln(c.Writer, "local_access.service_aliases:")
	if len(d.cfg.LocalAccess.ServiceAliases) == 0 {
		fmt.Fprintln(c.Writer, "  (none)")
	} else {
		for _, alias := range d.cfg.LocalAccess.ServiceAliases {
			fmt.Fprintf(c.Writer, "  %s.%s -> configured port %d\n", alias.Name, alias.MachineAlias, alias.Port)
		}
	}
	return nil
}
