//go:build !darwin && !linux

package daemoncmd

import "github.com/spf13/cobra"

func addDeviceGuardPlatformCommands(command *cobra.Command) {}
