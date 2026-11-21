package operatorinfra

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var ErrRegionInUse = errors.New("region is still in use")

func (s *Service) DeleteRegion(ctx context.Context, actor, key, id, reason string) (Region, error) {
	var out Region
	if !validCommon(actor, key, id, reason) || !regionIdentifier.MatchString(id) {
		return out, ErrInvalid
	}
	in := RegionMutation{Action: "delete", ID: id, Reason: reason}
	err := s.mutate(ctx, actor, key, "region.delete", id, in, &out, func(ctx context.Context, tx *db.Tx) error {
		// Enrollment locks this same row before reserving a managed installation.
		err := tx.QueryRow(ctx, `SELECT id,display_name,enabled,revision,updated_at
FROM operator_regions WHERE id=$1 FOR UPDATE`, id).Scan(&out.ID, &out.DisplayName, &out.Enabled, &out.Revision, &out.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var used bool
		// Installations carry their region on the node row, even while the claim is pending.
		err = tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM control_tunnel_nodes WHERE region=$1 AND state<>'retired') OR
EXISTS (SELECT 1 FROM workspace_edge_sets WHERE $1=ANY(regions)) OR
EXISTS (SELECT 1 FROM selfhost_regions WHERE slug=$1)`, id).Scan(&used)
		if err != nil {
			return err
		}
		if used {
			return ErrRegionInUse
		}
		_, err = tx.Exec(ctx, `DELETE FROM operator_regions WHERE id=$1`, id)
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23503" {
			return ErrRegionInUse
		}
		return err
	})
	if err != nil {
		return Region{}, err
	}
	return out, nil
}
