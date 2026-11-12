//go:build windows

package updated

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// openConnectionsForAutomatic is the controller's seam for the user daemon's open connections.
var openConnectionsForAutomatic = readWindowsOpenConnections

// refuseWhileConnectionsAreOpen keeps an automatic update from silently ending open SSH connections and access
// forwards (replacing the user daemon closes them), as the Unix updater does: the update stays pending and is tried
// again at its next window. A manual update is checked by the CLI, which knows --force. A probe that cannot answer
// never blocks an update: the updater must work when the app is broken. The count is the daemon's (which already
// includes the SSH sessions the hostd worker bridges) plus the SSH connections into this machine that its carrier bridges.
func (c *windowsController) refuseWhileConnectionsAreOpen(ctx context.Context, version string) error {
	open, err := openConnectionsForAutomatic(ctx, c.config)
	if err != nil {
		liveconnections.RecordUnconfirmed("updater", err)
		return nil
	}
	if open.Total() == 0 {
		return nil
	}
	return &autoupdate.ActiveTerminalSessionsError{RequiredVersion: version, SSH: open.SSH, Forwards: open.Forwards}
}

// readWindowsOpenConnections asks the enrolled user's daemon over its local API pipe. The service (LocalSystem) is
// admitted read-only by the pipe's DACL and verifies that the pipe belongs to the configured owner.
func readWindowsOpenConnections(ctx context.Context, config WindowsConfig) (liveconnections.Counts, error) {
	stateDir, err := windowsLocalAPIStateDir(config.RuntimeStateRoot)
	if err != nil {
		return liveconnections.Counts{}, err
	}
	paths, err := localapi.WindowsPaths(stateDir, config.OwnerSID)
	if err != nil {
		return liveconnections.Counts{}, err
	}
	open, daemonErr := localapi.ReadSystemOwnerConnections(ctx, paths.SocketPath, config.OwnerSID, localapi.OpenConnectionsTimeout)
	var incoming int
	var carrierErr error
	if instance, err := hostinstall.WindowsInstanceForSID(config.OwnerSID); err != nil {
		carrierErr = err
	} else {
		incoming, carrierErr = hostinstall.CarrierOpenSSH(ctx, instance)
	}
	return liveconnections.Combine(liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, daemonErr, incoming, carrierErr)
}
