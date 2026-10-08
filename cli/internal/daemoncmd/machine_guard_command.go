package daemoncmd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

func AddMachineGuardCommand(root *cobra.Command) {
	command := &cobra.Command{Use: "machine-guard", Short: "Manage protected local machine access"}
	run := &cobra.Command{Use: "run", Short: "Run the machine guard under service supervision", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return machineguard.Serve(cmd.Context(), machineguard.Config{})
	}}
	install := &cobra.Command{Use: "install", Short: "Install the root-owned machine guard service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err := machineguard.Install(cmd.Context(), executable); err != nil {
			return err
		}
		return writeDaemonCommandResult(cmd, map[string]any{"installed": true}, "Machine guard installed.")
	}}
	install.Flags().Bool("json", false, "print JSON")
	uninstall := newMachineGuardUninstallCommand(machineguard.Uninstall)
	command.AddCommand(run, install, uninstall)
	addMachineGuardPlatformCommands(command)
	root.AddCommand(command)
}

func newMachineGuardUninstallCommand(remove func(context.Context) (machineguard.UninstallResult, error)) *cobra.Command {
	command := &cobra.Command{Use: "uninstall", Short: "Remove machine access while retaining cached-address protection", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := remove(cmd.Context())
		message := "Machine access removed; address protection retained."
		if err == nil && len(result.Removed) == 0 && len(result.Retained) == 0 {
			message = "No machine guard installation found."
		}
		if err != nil {
			message = "Machine access removal incomplete; retry uninstall."
		}
		if len(result.Retained) > 0 {
			message += " Retained: " + strings.Join(result.Retained, "; ") + "."
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var outputErr error
		if err != nil && jsonOutput {
			outputErr = json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				SchemaVersion string                       `json:"schema_version"`
				OK            bool                         `json:"ok"`
				Data          machineguard.UninstallResult `json:"data"`
				Error         string                       `json:"error"`
			}{"1.0", false, result, err.Error()})
		} else {
			outputErr = writeDaemonCommandResult(cmd, result, message)
		}
		return errors.Join(err, outputErr)
	}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}
