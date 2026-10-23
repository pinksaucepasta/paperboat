package billing

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// EnsureSeats buys seats, as owner, until the team has at least quantity (directory sync and JIT
// provisioning, PLAN F19-05: "seats are added automatically, charged as a normal seat change").
// It does nothing when the team already has enough.
func (s *WorkspaceService) EnsureSeats(ctx context.Context, owner, team string, quantity int, key string) error {
	var current int
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, err := WorkspaceTx(ctx, tx, owner, team, true)
		current = w.Quantity
		return err
	})
	if err != nil || current >= quantity {
		return err
	}
	_, err = s.ChangeSeats(ctx, owner, team, quantity, key)
	return err
}
