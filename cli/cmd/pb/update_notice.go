package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/spf13/cobra"
)

func showLocalUpdateNotice(command *cobra.Command) {
	if updateJSON(command) || updatePolicyRecoveryCommand(command) {
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
