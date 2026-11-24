//go:build !windows

package main

import (
	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/spf13/cobra"
)

func executeManagedSSH(cobraCommand *cobra.Command, _ *command.Context, _ api.UserMachine, destination managedssh.Destination, passthrough []string, includePassthrough bool, environment []string) error {
	arguments := openSSHArguments(destination, passthrough, includePassthrough)
	if err := verifyManagedSSHLaunch(cobraCommand.Context(), destination, "ssh", arguments, environment); err != nil {
		return err
	}
	return (managedssh.OpenSSHExecutor{}).Execute(cobraCommand.Context(), "ssh", arguments, environment)
}
