package daemoncmd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/deviceguard"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

func AddDeviceGuardCommand(root *cobra.Command) {
	command := &cobra.Command{Use: "device-guard", Short: "Manage protected device-name access"}
	run := &cobra.Command{Use: "run", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return deviceguard.Serve(cmd.Context(), deviceguard.Config{ConfigureResolver: true})
	}}
	install := &cobra.Command{Use: "install", Short: "Install the root-owned device guard service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		loopbackCIDR, err := cmd.Flags().GetString("loopback-cidr")
		if err != nil {
			return err
		}
		if err := deviceguard.Install(cmd.Context(), executable, loopbackCIDR); err != nil {
			return err
		}
		return writeDaemonCommandResult(cmd, map[string]any{"installed": true}, "Device guard installed.")
	}}
	install.Flags().Bool("json", false, "print JSON")
	install.Flags().String("loopback-cidr", "", "validated local device loopback /16")
	uninstall := newDeviceGuardUninstallCommand(deviceguard.Uninstall)
	command.AddCommand(run, install, uninstall)
	addDeviceGuardPlatformCommands(command)
	root.AddCommand(command)
}

func newDeviceGuardUninstallCommand(remove func(context.Context) (deviceguard.UninstallResult, error)) *cobra.Command {
	command := &cobra.Command{Use: "uninstall", Short: "Remove device access while retaining cached-address protection", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := remove(cmd.Context())
		message := "Device access removed; address protection retained."
		if err == nil && len(result.Removed) == 0 && len(result.Retained) == 0 {
			message = "No device guard installation found."
		}
		if err != nil {
			message = "Device access removal incomplete; retry uninstall."
		}
		if len(result.Retained) > 0 {
			message += " Retained: " + strings.Join(result.Retained, "; ") + "."
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var outputErr error
		if err != nil && jsonOutput {
			outputErr = json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				SchemaVersion string                      `json:"schema_version"`
				OK            bool                        `json:"ok"`
				Data          deviceguard.UninstallResult `json:"data"`
				Error         string                      `json:"error"`
			}{"1.0", false, result, err.Error()})
		} else {
			outputErr = writeDaemonCommandResult(cmd, result, message)
		}
		return errors.Join(err, outputErr)
	}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}
