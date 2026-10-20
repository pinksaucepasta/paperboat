//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// F14-02: replacing the daemon ends open SSH connections and access forwards, so an update that
// would do that refuses (and a scheduled one stays pending) unless forced.
func TestUpdateRefusesWhileSSHConnectionsAreOpen(t *testing.T) {
	participants := &participantFixture{open: liveconnections.Counts{SSH: 2, Forwards: 1}}
	service := &Service{config: Config{Participants: participants}}

	err := service.refuseWhileConnectionsAreOpen(context.Background(), "2026.10.21.1", false)
	var busy *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &busy) || busy.RequiredVersion != "2026.10.21.1" || busy.SSH != 2 || busy.Forwards != 1 {
		t.Fatalf("an update with open connections was not refused: %v", err)
	}
	if want := "Paperboat is busy: 2 open SSH connections and 1 open port forward. Wait, close them, or use --force."; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if controlErrorCode(err) != autoupdate.BlockedActiveTerminalSessions {
		t.Fatalf("control code = %q: the scheduler defers on this code", controlErrorCode(err))
	}

	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "2026.10.21.1", true); err != nil {
		t.Fatalf("--force must proceed: %v", err)
	}

	participants.open = liveconnections.Counts{SSH: 1}
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err == nil || !strings.Contains(err.Error(), "1 open SSH connection.") {
		t.Fatalf("singular message = %v", err)
	}

	// Idle: the update installs. A probe that cannot answer never blocks one.
	participants.open = liveconnections.Counts{}
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err != nil {
		t.Fatalf("an idle machine was refused: %v", err)
	}
	participants.open, participants.openErr = liveconnections.Counts{SSH: 3}, errors.New("probe failed")
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err != nil {
		t.Fatalf("a failing probe blocked the update: %v", err)
	}
}
