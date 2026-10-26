package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/spf13/cobra"
)

func showLocalUpdateNotice(command *cobra.Command) {
	if updateJSON(command) || updatePolicyRecoveryCommand(command) || sshConnectionCommand(command) {
		return
	}
	ctx, cancel := context.WithTimeout(command.Context(), 300*time.Millisecond)
	defer cancel()
	client, err := newUpdateControlClient(300 * time.Millisecond)
	if err != nil {
		return
	}
	response, err := client.Status(ctx)
	if err != nil {
		return
	}
	if response.Settings != nil && !response.Settings.Enabled {
		return
	}
	writeOwnerMaintenanceNotice(command.ErrOrStderr(), response.OwnerMaintenance)
}

func writeOwnerMaintenanceNotice(output io.Writer, notice *autoupdate.OwnerMaintenanceNotice) {
	if notice == nil {
		return
	}
	fmt.Fprintf(output, "Paperboat %s is waiting for active sessions to close. Use `pb update --force` only if you want to interrupt them.\n", notice.Version)
}

// sshConnectionCommand is pb ssh, which OpenSSH tools and IDEs start per connection: the updater's status
// (~0.2 s, it validates the installed source) would be most of a warm connection's setup (F5-07).
func sshConnectionCommand(command *cobra.Command) bool {
	path := strings.Fields(command.CommandPath())
	return len(path) == 2 && path[1] == "ssh"
}
