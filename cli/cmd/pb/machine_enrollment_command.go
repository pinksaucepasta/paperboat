package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/machinename"
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
		posix, err := client.StartMachineEnrollment(ctx.Context, newIdempotencyKey(), "posix")
		if err != nil {
			return friendlyCommandError(err)
		}
		windows, err := client.StartMachineEnrollment(ctx.Context, newIdempotencyKey(), "powershell")
		if err != nil {
			return cancelEnrollmentCommands(ctx.Context, client, err, posix)
		}
		installerURL := strings.TrimRight(base.String(), "/") + "/install"
		makeURL := func(token string) string {
			parameter := token
			if name != "" {
				parameter = name + "-" + parameter
			}
			return installerURL + "?p=" + url.QueryEscape(parameter)
		}
		linuxCommand := "curl -fsSL '" + makeURL(posix.BootstrapToken) + "' | bash"
		windowsCommand := windowsEnrollmentCommand(makeURL(windows.BootstrapToken))
		expires := posix.ExpiresAt
		if windows.ExpiresAt.Before(expires) {
			expires = windows.ExpiresAt
		}
		asJSON, _ := command.Flags().GetBool("json")
		if asJSON {
			err = writeCLIJSON(command.OutOrStdout(), map[string]any{"expires_at": expires, "linux_macos_command": linuxCommand, "windows_command": windowsCommand})
		} else {
			_, err = fmt.Fprintf(command.OutOrStdout(), "Linux/macOS:\n%s\n\nWindows (PowerShell or Command Prompt):\n%s\n", linuxCommand, windowsCommand)
		}
		if err != nil {
			return cancelEnrollmentCommands(ctx.Context, client, err, posix, windows)
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
