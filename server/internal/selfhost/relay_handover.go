package selfhost

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// handOverRelays stores an account's relay pool and makes the change make-before-break. The relays the new pool no
// longer includes are listed in relay_pool_handovers: each keeps the account's carriers until the grace has passed
// (relaylimits.PoolHandoverGrace, long enough for the daemons to take the new relay set), then closes them with the
// non-fatal overload code so the clients reconnect through the new relays. Relays the pool includes again drop
// their entry, so switching back cancels the handover.
func handOverRelays(ctx context.Context, tx *db.Tx, account string, p Policy) error {
	eligibleRelays := `SELECT n.id FROM paperboat.control_tunnel_nodes n WHERE n.roles @> ARRAY['relay']::text[] AND n.state<>'retired' AND paperboat.node_pool_eligible(n.id,$1,'relay') ORDER BY n.id`
	rows, err := tx.Query(ctx, eligibleRelays, account)
	if err != nil {
		return err
	}
	var before []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		before = append(before, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'relay',$2,$3::text[]) ON CONFLICT(scope_kind,scope_id,capability) DO UPDATE SET mode=excluded.mode,installation_ids=excluded.installation_ids`, account, p.Mode, p.InstallationIDs); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM paperboat.relay_pool_handovers WHERE account_id=$1 AND paperboat.node_pool_eligible(node_id,$1,'relay')`, account); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO paperboat.relay_pool_handovers(node_id,account_id,close_at) SELECT id,$1,now()+make_interval(secs=>$3::float8) FROM unnest($2::text[]) AS id WHERE NOT paperboat.node_pool_eligible(id,$1,'relay') ON CONFLICT DO NOTHING`, account, before, relaylimits.PoolHandoverGrace.Seconds())
	return err
}
