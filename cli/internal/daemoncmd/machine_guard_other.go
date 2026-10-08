//go:build !darwin && !linux

package daemoncmd

import "github.com/spf13/cobra"

func addMachineGuardPlatformCommands(command *cobra.Command) {}
