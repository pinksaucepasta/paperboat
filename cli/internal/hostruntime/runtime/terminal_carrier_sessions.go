//go:build darwin || linux || windows

package runtime

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// A delegated transport continues existing PTYs only. New process launches go
// through the primary worker, which resolves current managed environment and
// launch policy. An update must never launch a replacement shell implicitly.
type continuingSessions struct{ session.Service }

func (s continuingSessions) Create(ctx context.Context, request session.CreateRequest) (session.Snapshot, error) {
	if _, err := s.Snapshot(request.ID); err != nil {
		return session.Snapshot{}, err
	}
	return session.Snapshot{}, session.ErrSessionExists
}

func (s continuingSessions) Restart(string) (session.Snapshot, error) {
	return session.Snapshot{}, session.ErrInvalidTransition
}
func (s continuingSessions) RestartAtGeneration(string, uint64) (session.Snapshot, error) {
	return session.Snapshot{}, session.ErrInvalidTransition
}
func (s continuingSessions) RestartContext(context.Context, string) (session.Snapshot, error) {
	return session.Snapshot{}, session.ErrInvalidTransition
}
func (s continuingSessions) RestartAtGenerationContext(context.Context, string, uint64) (session.Snapshot, error) {
	return session.Snapshot{}, session.ErrInvalidTransition
}
