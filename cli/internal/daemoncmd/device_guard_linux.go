//go:build linux

package daemoncmd

import (
	"github.com/pinksaucepasta/paperboat/internal/deviceguard"
	"github.com/spf13/cobra"
)

func addDeviceGuardPlatformCommands(command *cobra.Command) {
	command.AddCommand(&cobra.Command{Use: "restore-deny", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return deviceguard.RestoreDeny(cmd.Context()) }})
}
