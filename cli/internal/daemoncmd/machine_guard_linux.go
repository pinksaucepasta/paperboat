//go:build linux

package daemoncmd

import (
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/spf13/cobra"
)

func addMachineGuardPlatformCommands(command *cobra.Command) {
	command.AddCommand(&cobra.Command{Use: "restore-deny", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return machineguard.RestoreDeny(cmd.Context()) }})
}
