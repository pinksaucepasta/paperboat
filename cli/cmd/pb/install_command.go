package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/spf13/cobra"
)

var installSuppliedExecutable = hostruntimecmd.InstallRunningBinary
var runningInstallSource = installsource.Current

func platformInstallCommand() *cobra.Command {
	command := &cobra.Command{Use: "install", Short: "Install this executable and its local service", Args: commandArgs(cobra.NoArgs), SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			executable, source, err := runningInstallSource()
			if err != nil {
				return err
			}
			directory, _ := c.Flags().GetString("install-dir")
			ctx, cancel := context.WithTimeout(c.Context(), 3*time.Minute)
			defer cancel()
			installed, err := installSuppliedExecutable(ctx, executable, source, directory)
			var trustPending *hostruntimecmd.BrowserTrustPendingError
			if err != nil && !errors.As(err, &trustPending) {
				return err
			}
			result := map[string]any{"executable": installed, "version": source.Version, "distribution": source.Distribution, "automatic_update_checks": source.AutomaticUpdates, "sha256": source.SHA256, "enrollment": "unchanged"}
			if trustPending != nil {
				result["browser_https"] = "trust_pending"
				result["warnings"] = []string{trustPending.Error()}
				if _, warningErr := fmt.Fprintln(c.ErrOrStderr(), trustPending.Error()); warningErr != nil {
					return warningErr
				}
			}
			asJSON, _ := c.Flags().GetBool("json")
			if asJSON {
				return writeCLIJSON(c.OutOrStdout(), result)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Installed Paperboat %s at %s. Background update checks: %t. Enrollment is unchanged.\n", source.Version, installed, source.AutomaticUpdates)
			return err
		}}
	command.Flags().String("install-dir", "", "absolute directory for the Unix pb command")
	command.Flags().Bool("json", false, "print installation result as JSON")
	return command
}
