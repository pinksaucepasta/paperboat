//go:build darwin || linux

package updated

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
)

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
	if err != nil || open.Total() == 0 {
		return nil
	}
	return &autoupdate.ActiveTerminalSessionsError{RequiredVersion: version, SSH: open.SSH, Forwards: open.Forwards}
}
