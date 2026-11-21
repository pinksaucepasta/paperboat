package main

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
	"github.com/spf13/cobra"
)

func suppliedInstallTimeout(platform string) time.Duration {
	if platform == "windows" {
		return elevation.RuntimeInstallDuration
	}
	return 3 * time.Minute
}

func platformInstallCommand() *cobra.Command {
	command := &cobra.Command{Use: "install", Short: "Install this executable and its local service", Args: commandArgs(cobra.NoArgs), SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			executable, source, err := installDependenciesOf(c.Context()).runningSource()
			if err != nil {
				return err
			}
			directory, _ := c.Flags().GetString("install-dir")
			ctx, cancel := context.WithTimeout(c.Context(), suppliedInstallTimeout(runtime.GOOS))
			defer cancel()
			if err := windowsUninstallInFlight(ctx); err != nil {
				return err
			}
			installed, err := installDependenciesOf(ctx).installExecutable(ctx, executable, source, directory, selectedInstallServer(c))
			if err != nil {
				return err
			}
			result := map[string]any{"executable": installed, "version": source.Version, "distribution": source.Distribution, "automatic_update_checks": source.AutomaticUpdates, "sha256": source.SHA256, "enrollment": "unchanged"}
			warnings := installedTrustWarnings(c.Context())
			if len(warnings) > 0 {
				result["browser_https"] = "trust_pending"
			}
			warnings = append(warnings, managedSSHInstallWarnings()...)
			if len(warnings) > 0 {
				result["warnings"] = warnings
			}
			asJSON, _ := c.Flags().GetBool("json")
			if asJSON {
				return writeCLIJSON(c.OutOrStdout(), result)
			}
			for _, warning := range warnings {
				if _, err := fmt.Fprintln(c.ErrOrStderr(), warning); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Installed Paperboat %s at %s. Background update checks: %t. Enrollment is unchanged.\n", source.Version, installed, source.AutomaticUpdates)
			return err
		}}
	command.Flags().String("install-dir", "", "absolute directory for the Unix pb command")
	command.Flags().Bool("json", false, "print installation result as JSON")
	return command
}

// selectedInstallServer is the server chosen with --server, normalized, for the privileged install step
// (which learns the local access domain from it); empty uses the configured server (the install
// read the build's default server and failed against a staging server).
func selectedInstallServer(command *cobra.Command) string {
	server, err := config.NormalizeServerURL(workspaceServerOverride(command))
	if err != nil {
		return ""
	}
	return server
}
