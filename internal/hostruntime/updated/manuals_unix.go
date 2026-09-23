//go:build darwin || linux

package updated

import (
	"context"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// Manuals change only after the durable runtime commit. Before that boundary,
// rollback leaves the previous manual set untouched. A partial extraction after
// commit is retried by both ordinary startup and persistent-helper recovery.
type manualCommitGate struct {
	workerupdate.ActivationGate
	refresh func(context.Context, workerupdate.Release) error
}

func (g *manualCommitGate) Commit(ctx context.Context, request workerupdate.GateRequest) error {
	if err := g.ActivationGate.Commit(ctx, request); err != nil {
		return err
	}
	if err := g.refresh(ctx, request.Candidate); err != nil {
		return fmt.Errorf("runtime committed; bundled manual refresh remains pending and will retry during update recovery: %w", err)
	}
	return nil
}

func (s *Service) manualCommitGate(inner workerupdate.ActivationGate) workerupdate.ActivationGate {
	if s.config.RefreshManuals == nil {
		return inner
	}
	return &manualCommitGate{ActivationGate: inner, refresh: func(ctx context.Context, release workerupdate.Release) error {
		// The recovery helper may itself be the old release. Execute only the
		// exact protected candidate bytes selected by the authenticated journal.
		if err := verifyUnixExecutable(s.config.Binary, release); err != nil {
			return err
		}
		return s.config.RefreshManuals(ctx)
	}}
}
