//go:build windows

package main

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// daemonConnectionsForUpdate counts the SSH streams and access forwards the user's daemon carries, over its local API
// pipe like the Unix probe.
func daemonConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return liveconnections.Counts{}, err
	}
	return openConnectionsOver(ctx, paths.SocketPath)
}

// carrierSSHForUpdate counts the SSH connections into this machine that the user's carrier bridges.
func carrierSSHForUpdate(ctx context.Context) (int, error) {
	sid, err := localdaemon.CurrentUserSID()
	if err != nil {
		return 0, err
	}
	instance, err := hostinstall.WindowsInstanceForSID(sid)
	if err != nil {
		return 0, err
	}
	return hostinstall.CarrierOpenSSH(ctx, instance)
}

// openConnectionsOver asks the daemon listening on the pipe. A daemon that is not running (no pipe to dial) carries none.
func openConnectionsOver(ctx context.Context, socketPath string) (liveconnections.Counts, error) {
	client, err := localapi.NewClient(socketPath, localapi.OpenConnectionsTimeout)
	if err != nil {
		return liveconnections.Counts{}, err
	}
	open, err := client.OpenConnections(ctx)
	if errors.Is(err, localapi.ErrTransportUnavailable) {
		return liveconnections.Counts{}, nil
	}
	return liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, err
}
