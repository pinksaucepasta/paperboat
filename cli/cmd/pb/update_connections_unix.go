//go:build darwin || linux

package main

import (
	"context"
	"os"
	"strconv"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
)

func daemonConnectionsForUpdate(ctx context.Context) (liveconnections.Counts, error) {
	open, err := localdaemon.OpenConnectionsCurrentUser(ctx)
	return liveconnections.Counts{SSH: open.SSH, Forwards: open.Forwards}, err
}

func carrierSSHForUpdate(ctx context.Context) (int, error) {
	return hostinstall.CarrierOpenSSH(ctx, "u"+strconv.Itoa(os.Geteuid()))
}
