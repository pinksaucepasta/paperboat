//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"os"
	"strconv"
)

// RepairManagedHost asks the fixed installed runtime binary to recover its
// persisted lifecycle transaction and re-apply the exact prior declaration.
// No caller-controlled installation payload crosses the elevation boundary.
func RepairManagedHost(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	command := sudoCommand(ctx, "--", "/usr/bin/env",
		"PAPERBOAT_INVOKING_UID="+strconv.Itoa(os.Getuid()), systemWorkerExecutable(), "__runtime-service", "repair-persisted")
	reason := &tailBuffer{limit: 2048}
	command.Stderr = reason
	if err := command.Run(); err != nil {
		if refused := sudoRefusal("Repairing the Paperboat system service", err, reason.String()); refused != nil {
			return refused
		}
		return runtimeFailure("administrator approval or host service repair failed", err, runtimeContextFailure(ctx))
	}
	return nil
}
