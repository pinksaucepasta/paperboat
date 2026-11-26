package main

import (
	"context"
	"fmt"
	"os"
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
			if err := refuseMacRootInstall(runtime.GOOS, os.Geteuid()); err != nil {
				return err
			}
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
			// Install never signs in or pairs: say where this computer stands and the next step (2026-11-26: "Enrollment
			// is unchanged" on a revoked computer hid that only `pb login` would bring it back).
			next := "Sign-in and pairing are unchanged; `pb status` shows them."
			if installDependenciesOf(c.Context()).awaitingEnrollment(c.Context()) {
				next = "This computer is not signed in to Paperboat; run `pb login` to add it to your account."
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Installed Paperboat %s at %s. Background update checks: %t. %s\n", source.Version, installed, source.AutomaticUpdates, next)
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

// refuseMacRootInstall stops `pb install`, `pb pair` and `pb update` as root on macOS before any step: the install is per user (launchd
// agents in the user's session), and a root install failed midway at the machine guard step.
func refuseMacRootInstall(platform string, euid int) error {
	if platform == "darwin" && euid == 0 {
		return commandRejection{reason: commandRejectMacRootInstall}
	}
	return nil
}
