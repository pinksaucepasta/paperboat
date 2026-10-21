//go:build darwin || linux

package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// openConnectionsForUpdate counts the SSH streams and access forwards the daemon carries now.
func openConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	open, err := localdaemon.OpenConnectionsCurrentUser(ctx)
	return liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, err
}
