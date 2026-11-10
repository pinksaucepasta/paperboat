//go:build !darwin && !linux && !windows

package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// Only Linux, macOS and Windows count open connections.
func openConnectionsForUpdate(context.Context) (liveconnections.Counts, error) {
	return liveconnections.Counts{}, nil
}
