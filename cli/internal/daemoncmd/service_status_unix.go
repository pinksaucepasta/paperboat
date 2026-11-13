//go:build !windows

package daemoncmd

import (
	"context"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// hostServiceServing: the machine's host service (system LaunchDaemon/unit) answers its socket, which is
// what keeps the machine reachable before the user daemon runs.
var hostServiceServing = func(ctx context.Context) bool {
	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", hostservice.DefaultSocketPath())
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

// daemonServiceStatus reports what is running. The service manager can be unreachable for this session
// (Linux user bus gone after logout, macOS user LaunchAgent not loaded before first login) while the
// daemon or the machine's host services still serve, so the daemon's own socket decides "running".
func daemonServiceStatus(ctx context.Context, state localdaemon.ServiceState) (string, bool) {
	status := "not-installed"
	if state.Installed {
		status = "stopped"
	}
	if !state.Running {
		state.Running = daemonAnswers(ctx)
	}
	if state.Running {
		status = "running"
	}
	return status, hostServiceServing(ctx)
}

func daemonAnswers(ctx context.Context) bool {
	paths, err := servicePaths()
	if err != nil {
		return false
	}
	client, err := newLocalAPIClient(paths.SocketPath, time.Second)
	if err != nil {
		return false
	}
	_, err = client.Snapshot(ctx)
	return err == nil
}
