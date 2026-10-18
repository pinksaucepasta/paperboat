package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/spf13/cobra"
)

var installSuppliedExecutable = hostruntimecmd.InstallRunningBinary
var runningInstallSource = installsource.Current

func suppliedInstallTimeout(platform string) time.Duration {
	if platform == "windows" {
		return elevation.RuntimeInstallDuration
	}
	return 3 * time.Minute
}

func platformInstallCommand() *cobra.Command {
	command := &cobra.Command{Use: "install", Short: "Install this executable and its local service", Args: commandArgs(cobra.NoArgs), SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			executable, source, err := runningInstallSource()
			if err != nil {
				return err
			}
			directory, _ := c.Flags().GetString("install-dir")
			ctx, cancel := context.WithTimeout(c.Context(), suppliedInstallTimeout(runtime.GOOS))
			defer cancel()
			installed, err := installSuppliedExecutable(ctx, executable, source, directory)
			if err != nil {
				var refused *hostinstall.SuppliedExecutableError
				if errors.As(err, &refused) {
					return usageError{err: err, publicMessage: refused.Error()}
				}
				return err
			}
			result := map[string]any{"executable": installed, "version": source.Version, "distribution": source.Distribution, "automatic_update_checks": source.AutomaticUpdates, "sha256": source.SHA256, "enrollment": "unchanged"}
			warnings := installedTrustWarnings(c.Context())
			if len(warnings) > 0 {
				result["browser_https"] = "trust_pending"
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
