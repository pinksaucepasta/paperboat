//go:build !windows

package main

import (
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

func executeManagedSSH(cobraCommand *cobra.Command, _ *command.Context, _ api.UserMachine, destination managedssh.Destination, passthrough []string, includePassthrough bool, environment []string) error {
	return (managedssh.OpenSSHExecutor{}).Execute(cobraCommand.Context(), "ssh", openSSHArguments(destination, passthrough, includePassthrough), environment)
}
