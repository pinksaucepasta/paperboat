//go:build windows

package main

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// openConnectionsForUpdate counts the SSH streams and access forwards the user's daemon carries now, over its local API
// pipe like the Unix probe.
func openConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return liveconnections.Counts{}, err
	}
	return openConnectionsOver(ctx, paths.SocketPath)
}

// openConnectionsOver asks the daemon listening on the pipe. A daemon that is not running (no pipe to dial) carries none.
func openConnectionsOver(ctx context.Context, socketPath string) (liveconnections.Counts, error) {
	client, err := localapi.NewClient(socketPath, 2*time.Second)
	if err != nil {
		return liveconnections.Counts{}, err
	}
	open, err := client.OpenConnections(ctx)
	if errors.Is(err, localapi.ErrTransportUnavailable) {
		return liveconnections.Counts{}, nil
	}
	return liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, err
}
