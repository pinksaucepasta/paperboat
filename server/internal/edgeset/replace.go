package edgeset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// ErrRegionEnabled stops ReplaceRegion: the region was enabled again while it ran.
var ErrRegionEnabled = errors.New("region was enabled again")

// replaceScopesSQL pages through the workspaces whose stored pair holds region $1.
const replaceScopesSQL = `SELECT e.scope_kind,e.scope_id,CASE WHEN e.scope_kind='team' THEN t.owner_account ELSE e.scope_id END
FROM workspace_edge_sets e LEFT JOIN teams t ON e.scope_kind='team' AND t.team_id=e.scope_id
WHERE $1=ANY(e.regions) AND (e.scope_kind,e.scope_id)>($2,$3) AND (e.scope_kind<>'team' OR t.team_id IS NOT NULL)
ORDER BY e.scope_kind,e.scope_id LIMIT $4`

// ReplaceRegion moves every workspace whose pair holds region onto other regions now, one transaction per workspace,
// instead of waiting for the sweep to see the region's nodes gone, and wakes each moved workspace's machines (the wake is queued with the move). A
// workspace with no other usable region keeps its pair (ReconcileScope's rule) and is returned in kept; running it
// again after a full move finds nothing, so a replay wakes nobody.
func ReplaceRegion(ctx context.Context, database *db.DB, region string, now time.Time) (moved, kept int, err error) {
	const page = 100
	kind, id := "", ""
	for {
		var scopes []Scope
		rows, err := database.Pool().Query(ctx, replaceScopesSQL, region, kind, id, page)
		if err != nil {
			return moved, kept, fmt.Errorf("list workspaces in region: %w", err)
		}
		for rows.Next() {
			var s Scope
			if err = rows.Scan(&s.Kind, &s.ID, &s.Account); err != nil {
				break
			}
			scopes = append(scopes, s)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return moved, kept, fmt.Errorf("list workspaces in region: %w", err)
		}
		for _, scope := range scopes {
			rtt, err := ReadLatency(ctx, database.Pool(), scope, now)
			if err != nil {
				return moved, kept, fmt.Errorf("re-place %s %s: %w", scope.Kind, scope.ID, err)
			}
			var did bool
			err = database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				// An operator region re-enabled meanwhile stops the pass (FOR SHARE holds off a concurrent enable).
				var enabled bool
				if err := tx.QueryRow(ctx, `SELECT enabled FROM operator_regions WHERE id=$1 FOR SHARE`, region).Scan(&enabled); err == nil && enabled {
					return ErrRegionEnabled
				} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if err := LockWorkspace(ctx, tx, scope); err != nil {
					return err
				}
				var inner error
				did, inner = reconcileScope(ctx, tx, scope, rtt, now, region)
				return inner
			})
			if err != nil {
				return moved, kept, fmt.Errorf("re-place %s %s: %w", scope.Kind, scope.ID, err)
			}
			if did {
				moved++
			} else {
				kept++
			}
			kind, id = scope.Kind, scope.ID
		}
		DrainWakesLogged(ctx, database)
		if len(scopes) < page {
			return moved, kept, nil
		}
	}
}
