//go:build windows

package daemoncmd

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// Windows asks the service manager, which is machine-wide and not tied to a session, so it is already the
// real state.
func daemonServiceStatus(_ context.Context, state localdaemon.ServiceState) (string, bool) {
	status := "not-installed"
	if state.Installed {
		status = "stopped"
	}
	if state.Running {
		status = "running"
	}
	return status, false
}
