//go:build darwin || linux

package updated

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// workerOpenSSH asks the worker for the managed SSH connections it bridges itself (the
// operation-based path); the participants report the daemon's and the carrier's.
func (s *Service) workerOpenSSH(ctx context.Context) (int, error) {
	owner, err := hostdproto.NewClient(s.config.SocketPath, s.config.Token, 5*time.Second)
	if err != nil {
		return 0, err
	}
	return owner.OpenSSH(ctx)
}

// refuseWhileConnectionsAreOpen keeps an update from silently ending open SSH connections and
// access forwards: replacing the user daemon closes them. A manual update says how many are open and
// that --force overrides; an automatic update is left pending like one waiting for terminal sessions
// and tries again at its next window, so nothing is ever killed unasked.
// A probe that cannot answer never blocks an update: the updater must work when the app is broken.
func (s *Service) refuseWhileConnectionsAreOpen(ctx context.Context, version string, force bool) error {
	if force {
		return nil
	}
	open, err := s.config.Participants.OpenConnections(ctx)
	if err != nil {
		open = liveconnections.Counts{}
	}
	if worker, workerErr := s.workerOpenSSH(ctx); workerErr == nil {
		open.SSH += worker
	}
	if open.Total() == 0 {
		return nil
	}
	return &autoupdate.ActiveTerminalSessionsError{RequiredVersion: version, SSH: open.SSH, Forwards: open.Forwards}
}
