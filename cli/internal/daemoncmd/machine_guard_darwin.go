//go:build darwin

package daemoncmd

import (
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/spf13/cobra"
)

func addMachineGuardPlatformCommands(command *cobra.Command) {
	command.AddCommand(&cobra.Command{Use: "socket <address> <loopback-cidr>", Hidden: true, Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, args []string) error {
		return machineguard.ServeSocketChild(command.Context(), args[0], args[1])
	}})
	command.AddCommand(&cobra.Command{Use: "deny-only", Hidden: true, Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return machineguard.ServeDenyOnly(command.Context()) }})
}
