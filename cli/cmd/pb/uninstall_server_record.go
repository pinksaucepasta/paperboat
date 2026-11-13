package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
)

const uninstallServerRecordTimeout = 15 * time.Second

// disconnectOwnMachine removes this computer's machine record on the server (the call pb machine revoke makes),
// so peers drop it at once instead of listing an uninstalled machine as offline forever (X-41). A computer that
// was never added has no record: nothing to do.
var disconnectOwnMachine = func(c *command.Context) error {
	machineID, err := configuredMachineID()
	if err != nil {
		var preparation commandPreparationFailure
		if errors.As(err, &preparation) && preparation.step == prepareMachineNotAdded {
			return nil
		}
		return err
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Context, uninstallServerRecordTimeout)
	defer cancel()
	if err := client.DisconnectUserMachine(ctx, machineID); err != nil && !api.IsNotFound(err) {
		return err
	}
	return nil
}

// removeOwnServerRecord runs before any local removal. A failure never stops the uninstall: it returns the
// sentence telling the user how to remove the record themselves, "" when the record is gone.
func removeOwnServerRecord(c *command.Context) string {
	err := disconnectOwnMachine(c)
	if err == nil {
		return ""
	}
	reason := "the Paperboat server could not be reached"
	if !api.Unreachable(err) {
		reason = fmt.Sprintf("it failed (%v)", err)
	}
	return "This machine's record was not removed from your account because " + reason +
		". Uninstall continues locally; remove the machine from the console (Machines, Remove) or run pb machine revoke from another machine."
}
