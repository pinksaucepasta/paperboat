package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// signedInDaemonRefreshTimeout bounds starting the daemon and its first inventory refresh after a
// sign-in. The refresh walks the account's machines, which takes several seconds on a large account
// when the control plane is slow; a timeout here leaves the sign-in saved but the login failed.
const signedInDaemonRefreshTimeout = 60 * time.Second

func refreshSignedInDaemon(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil || !matches {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, signedInDaemonRefreshTimeout)
	defer cancel()
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	if err != nil {
		return err
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return err
	}
	client, err := localapi.NewClient(paths.SocketPath, signedInDaemonRefreshTimeout)
	if err != nil {
		return err
	}
	return client.RefreshInventory(ctx)
}
