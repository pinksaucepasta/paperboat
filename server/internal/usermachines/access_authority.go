package usermachines

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Lock the selected team, or every existing team binding of an unscoped
// machine, before inspecting the grant. Team and policy writers lock the team
// row before changing membership, bindings, or policy.
func lockAccessTeamsTx(ctx context.Context, tx *db.Tx, machine string) ([]string, error) {
	selected, team := workspaceTeamArgument(ctx)
	if selected && !team.Valid {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT t.team_id FROM teams t
 WHERE t.team_id=$2 OR (NOT $3 AND EXISTS (
 SELECT 1 FROM team_resource_bindings b
 WHERE b.team_id=t.team_id AND b.resource_kind='machine' AND b.resource_id=$1))
 ORDER BY t.team_id FOR SHARE OF t`, machine, team, selected)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Keep machine revocation and client-session updates ordered before the
// access write and its peer-identity updates.
func lockAccessEndpointsTx(ctx context.Context, tx *db.Tx, account, clientSession string, machineIDs ...string) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM user_machines WHERE id=ANY($1::text[]) ORDER BY id FOR SHARE`, machineIDs); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT id FROM cli_client_sessions WHERE id=$1 AND user_id=$2 FOR SHARE`, clientSession, account)
	return err
}
