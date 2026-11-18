package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

// daemonSnapshotLookupTimeout bounds the extra daemon read a failed command makes to explain itself.
const daemonSnapshotLookupTimeout = 2 * time.Second

var readDaemonSnapshot = func(ctx context.Context) (localapi.Snapshot, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, daemonSnapshotLookupTimeout)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}
