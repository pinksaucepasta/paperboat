package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func refreshSignedInDaemon(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil || !matches {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	if err != nil {
		return err
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return err
	}
	client, err := localapi.NewClient(paths.SocketPath, 15*time.Second)
	if err != nil {
		return err
	}
	return client.RefreshInventory(ctx)
}
