//go:build windows

package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// openConnectionsForUpdate counts the SSH streams and access forwards the user's daemon carries now, over its local API
// pipe like the Unix probe. A daemon that is not running (no pipe) or too old to know the question carries none: the
// answer never blocks an update that no connection can be shown to be blocking.
func openConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return liveconnections.Counts{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return liveconnections.Counts{}, err
	}
	open, err := client.OpenConnections(ctx)
	var remote *localapi.RemoteError
	if errors.As(err, &remote) && remote.StatusCode == http.StatusNotFound {
		return liveconnections.Counts{}, nil
	}
	return liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, err
}
