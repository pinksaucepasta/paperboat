//go:build !darwin && !linux

package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// The Windows updater does not yet count open connections.
func openConnectionsForUpdate(context.Context) (liveconnections.Counts, error) {
	return liveconnections.Counts{}, nil
}
