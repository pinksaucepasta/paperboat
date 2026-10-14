package teams

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// SetOperatorSuspensionTx changes team eligibility without changing membership,
// grants, encrypted material, or billing. The operator action owner writes its
// action receipt and audit event in the same surrounding transaction.
func SetOperatorSuspensionTx(ctx context.Context, tx *db.Tx, actor, target string, suspend bool, expectedGeneration int64) (int64, error) {
	if tx == nil || !validID(actor) || !validID(target) || expectedGeneration < 1 || uint64(expectedGeneration) >= MaximumCounter {
		return 0, ErrInvalid
	}
	if err := Lock(ctx, tx); err != nil {
		return 0, err
	}
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_accounts WHERE id=$1 AND status='active' AND role='admin')`, actor).Scan(&authorized); err != nil {
		return 0, err
	}
	if !authorized {
		return 0, ErrForbidden
	}
	var generation int64
	err := tx.QueryRow(ctx, `UPDATE teams SET suspended=$3,suspended_at=CASE WHEN $3 THEN now() ELSE NULL END,suspended_by=CASE WHEN $3 THEN $1 ELSE NULL END,generation=generation+1 WHERE team_id=$2 AND deleted_at IS NULL AND generation=$4 AND suspended<>$3 RETURNING generation`, actor, target, suspend, expectedGeneration).Scan(&generation)
	if err == pgx.ErrNoRows {
		var exists bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE team_id=$1 AND deleted_at IS NULL)`, target).Scan(&exists); e != nil {
			return 0, e
		}
		if !exists {
			return 0, ErrNotFound
		}
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if suspend {
		if err := FenceENVTx(ctx, tx, target); err != nil {
			return 0, err
		}
	} else if err := InvalidateENVTx(ctx, tx, target); err != nil {
		return 0, err
	}
	return generation, nil
}
