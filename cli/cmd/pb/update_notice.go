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
	fmt.Fprintf(output, "Paperboat %s needs a process-owner restart. Running terminals and commands will end at %s, after any in-progress operation finishes. Run `pb update settings --auto=false` to turn off automatic installation.\n", notice.Version, notice.Deadline.Local().Format("Jan 2, 15:04 MST"))
}
