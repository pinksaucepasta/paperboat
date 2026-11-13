//go:build darwin || linux

package daemoncmd

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// R-X-39: a logged-out Linux user (no user bus) printed not-installed while the daemon served exec.
func TestServiceStatusFollowsTheDaemonThatAnswers(t *testing.T) {
	oldPaths, oldHost := servicePaths, hostServiceServing
	t.Cleanup(func() { servicePaths, hostServiceServing = oldPaths, oldHost })
	hostServiceServing = func(context.Context) bool { return false }
	_, paths := startReadinessServer(t, readinessSnapshot(1, "ready"))
	servicePaths = func() (localapi.Paths, error) { return paths, nil }
	if status, host := daemonServiceStatus(t.Context(), localdaemon.ServiceState{}); status != "running" || host {
		t.Fatalf("daemon answers, manager unreachable: status=%s host=%v", status, host)
	}
	servicePaths = func() (localapi.Paths, error) { return localapi.Paths{SocketPath: t.TempDir() + "/none.sock"}, nil }
	hostServiceServing = func(context.Context) bool { return true }
	for _, test := range []struct {
		state localdaemon.ServiceState
		want  string
	}{{localdaemon.ServiceState{}, "not-installed"}, {localdaemon.ServiceState{Installed: true}, "stopped"}, {localdaemon.ServiceState{Installed: true, Running: true}, "running"}} {
		if status, host := daemonServiceStatus(t.Context(), test.state); status != test.want || !host {
			t.Fatalf("state %+v: status=%s host=%v want %s with host services", test.state, status, host, test.want)
		}
	}
}
