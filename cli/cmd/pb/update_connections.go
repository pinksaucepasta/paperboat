//go:build darwin || linux || windows

package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// The two probes an update asks, as variables so a test can answer for them.
var (
	readDaemonConnections = daemonConnectionsForUpdate
	readCarrierSSH        = carrierSSHForUpdate
)

// openConnectionsForUpdate counts what an update would cut: the SSH streams and access forwards the daemon carries as a
// client, and the SSH connections into this machine that its carrier bridges (F14-02).
func openConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	open, daemonErr := readDaemonConnections(ctx)
	incoming, carrierErr := readCarrierSSH(ctx)
	return liveconnections.Combine(open, daemonErr, incoming, carrierErr)
}
