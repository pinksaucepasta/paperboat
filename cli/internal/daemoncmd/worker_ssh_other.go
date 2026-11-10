//go:build !windows

package daemoncmd

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// runLocalDaemon starts the user daemon. The Unix updater service asks the worker for its SSH count itself.
func runLocalDaemon(ctx context.Context, config localdaemon.DaemonConfig) error {
	return localdaemon.Run(ctx, config)
}
