package teams

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var ErrSeatRequired = errors.New("buy a seat before inviting another member")

// SeatUsageTx shares the team mutation lock with invites, acceptance and billing.
// An accepted invitation exchanges its reservation for membership in one transaction.
func SeatUsageTx(ctx context.Context, tx *db.Tx, team string) (int, error) {
	var used int
	err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM team_members WHERE team_id=$1 AND active) +
 (SELECT count(*) FROM team_invitations WHERE team_id=$1 AND accepted_at IS NULL
 AND cancelled_at IS NULL AND expires_at>now())`, team).Scan(&used)
	return used, err
}

func reserveInvitationSeatTx(ctx context.Context, tx *db.Tx, team string) error {
	used, err := SeatUsageTx(ctx, tx, team)
	if err != nil {
		return err
	}
	var capacity int
	err = tx.QueryRow(ctx, `SELECT coalesce((SELECT least(quantity,coalesce(scheduled_quantity,quantity),coalesce(pending_seat_capacity,quantity))
 FROM billing_workspaces WHERE team_id=$1 AND team_paid_entitled($1)),0)`, team).Scan(&capacity)
	if err != nil {
		return err
	}
	if used >= capacity {
		return ErrSeatRequired
	}
	return nil
}
