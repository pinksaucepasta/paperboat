//go:build windows

package updated

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// F14-02 on Windows: an automatic update stays pending while connections are open and proceeds once they are closed.
func TestWindowsAutomaticUpdateStaysPendingWhileConnectionsAreOpen(t *testing.T) {
	previous := openConnectionsForAutomatic
	t.Cleanup(func() { openConnectionsForAutomatic = previous })
	counts := liveconnections.Counts{SSH: 2, Forwards: 1}
	var probeErr error
	openConnectionsForAutomatic = func(context.Context, WindowsConfig) (liveconnections.Counts, error) { return counts, probeErr }
	c := &windowsController{}

	err := c.refuseWhileConnectionsAreOpen(context.Background(), "2026.11.10.1")
	var busy *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &busy) || busy.RequiredVersion != "2026.11.10.1" || !strings.Contains(err.Error(), "2 open SSH connections and 1 open port forward") {
		t.Fatalf("open connections did not keep the update pending: %v", err)
	}
	if code := controlErrorCodeWindows(err); code != "active_terminal_sessions" {
		t.Fatalf("control code %q", code)
	}
	counts = liveconnections.Counts{}
	if err := c.refuseWhileConnectionsAreOpen(context.Background(), "v"); err != nil {
		t.Fatalf("closed connections still blocked the update: %v", err)
	}
	counts, probeErr = liveconnections.Counts{SSH: 5}, errors.New("daemon cannot answer")
	if err := c.refuseWhileConnectionsAreOpen(context.Background(), "v"); err != nil {
		t.Fatalf("a daemon that cannot answer blocked the update: %v", err)
	}
}

func TestWindowsLocalAPIStateDirIsTheRuntimeRootsSiblingAndOneRuleForTheLockPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	dir, err := windowsLocalAPIStateDir(root)
	lock, lockErr := windowsLocalDaemonLockPath(root)
	if err != nil || lockErr != nil || dir != filepath.Join(filepath.Dir(root), "state") || lock != filepath.Join(dir, "daemon.lock") {
		t.Fatalf("dir=%q err=%v lock=%q err=%v", dir, err, lock, lockErr)
	}
	if _, err := windowsLocalAPIStateDir("relative"); err == nil {
		t.Fatal("a relative runtime root was accepted")
	}
}
