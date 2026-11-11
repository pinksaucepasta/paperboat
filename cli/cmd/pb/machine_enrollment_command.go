package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/machinename"
	"github.com/spf13/cobra"
)

func machineEnrollmentCommand() *cobra.Command {
	command := &cobra.Command{Use: "add", Short: "Print Linux/macOS and Windows machine enrollment commands", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		name, _ := command.Flags().GetString("name")
		if name != "" {
			if err := machinename.Validate(name); err != nil {
				return invocationError(fmt.Errorf("invalid machine name: %w", err))
			}
		}
		// machine add is workspace-independent, so the flag is read here: the token is for the workspace the user named.
		workspace := ""
		if explicitWorkspaceSelection(command) {
			var err error
			if workspace, err = resolveWorkspaceInvocation(command); err != nil {
				return err
			}
		}
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		discovery, err := client.ClientConfiguration(ctx.Context)
		if err != nil {
			return friendlyCommandError(err)
		}
		base, err := url.Parse(discovery.InstallerBaseURL)
		if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			return errors.New("server discovery does not provide a valid installer URL; ask the server operator to configure installation")
		}
		enrollment, err := client.StartMachineEnrollmentIn(ctx.Context, newIdempotencyKey(), workspace)
		if err != nil {
			return friendlyCommandError(err)
		}
		// One token for every platform: the installer path says which shell runs it.
		makeURL := func(installer string) string {
			parameter := enrollment.BootstrapToken
			if name != "" {
				parameter = name + "-" + parameter
			}
			return strings.TrimRight(base.String(), "/") + installer + "?p=" + url.QueryEscape(parameter)
		}
		linuxCommand := "curl -fsSL '" + makeURL("/install") + "' | bash"
		windowsCommand := windowsEnrollmentCommand(makeURL("/install.ps1"))
		asJSON, _ := command.Flags().GetBool("json")
		if asJSON {
			err = writeCLIJSON(command.OutOrStdout(), map[string]any{"expires_at": enrollment.ExpiresAt, "linux_macos_command": linuxCommand, "windows_command": windowsCommand})
		} else {
			_, err = fmt.Fprintf(command.OutOrStdout(), "Linux/macOS:\n%s\n\nWindows (PowerShell or Command Prompt):\n%s\n", linuxCommand, windowsCommand)
		}
		if err != nil {
			return cancelEnrollmentCommands(ctx.Context, client, err, enrollment)
		}
		return nil
	}}
	command.Flags().String("name", "", "optional machine hostname")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func cancelEnrollmentCommands(ctx context.Context, client *api.Client, primary error, enrollments ...api.MachineEnrollmentStart) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	failures := []error{primary}
	for _, enrollment := range enrollments {
		if err := client.CancelMachineEnrollment(cleanup, enrollment.ID, newIdempotencyKey()); err != nil {
			failures = append(failures, err)
		}
	}
	return fmt.Errorf("machine enrollment commands could not be completed; unused enrollment cancellation attempted: %w", errors.Join(failures...))
}
