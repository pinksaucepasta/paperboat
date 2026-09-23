package daemoncmd

import (
	"context"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"github.com/spf13/cobra"
)

type deviceGuardTrustInstaller func(context.Context, string) error

func newDeviceGuardTrustCommand(install deviceGuardTrustInstaller) *cobra.Command {
	var suffix string
	command := &cobra.Command{
		Use:   "trust",
		Short: "Trust Paperboat private HTTPS names on this Mac",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			clean, err := splitdns.ValidateSuffix(suffix)
			if err != nil {
				return fmt.Errorf("invalid --suffix: %w", err)
			}
			if err := install(command.Context(), clean); err != nil {
				return fmt.Errorf("install Paperboat HTTPS trust for .%s: %w; recovery: run `sudo pb daemon device-guard trust --suffix %s` from an interactive macOS terminal", clean, err, clean)
			}
			fmt.Fprintf(command.OutOrStdout(), "Trusted Paperboat HTTPS names under .%s.\n", clean)
			return nil
		},
		SilenceUsage: true,
	}
	command.Flags().StringVar(&suffix, "suffix", "pprbt", "private Paperboat device suffix")
	return command
}
