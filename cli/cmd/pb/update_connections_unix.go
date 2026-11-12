//go:build darwin || linux

package main

import (
	"context"
	"os"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

func daemonConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	open, err := localdaemon.OpenConnectionsCurrentUser(ctx)
	return liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, err
}

func carrierSSHForUpdate(ctx context.Context) (int, error) {
	return hostinstall.CarrierOpenSSH(ctx, "u"+strconv.Itoa(os.Geteuid()))
}
