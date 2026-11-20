//go:build darwin || linux || windows

package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/cli/internal/liveconnections"
)

// openConnectionsForUpdate counts what an update would cut: the SSH streams and access forwards the daemon carries as a
// client, and the SSH connections into this machine that its carrier bridges (F14-02).
func openConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	return readUpdateConnections(ctx, daemonConnectionsForUpdate, carrierSSHForUpdate)
}

func readUpdateConnections(ctx context.Context, readDaemon func(context.Context) (liveconnections.Counts, error), readCarrier func(context.Context) (int, error)) (liveconnections.Counts, error) {
	open, daemonErr := readDaemon(ctx)
	incoming, carrierErr := readCarrier(ctx)
	return liveconnections.Combine(open, daemonErr, incoming, carrierErr)
}
