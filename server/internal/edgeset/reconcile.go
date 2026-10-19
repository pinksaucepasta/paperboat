package edgeset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Scope names a workspace: a personal account or a team. Account is any member that can stand for
// the workspace when asking which edges it may use (the owner).
type Scope struct {
	Kind    string
	ID      string
	Account string
}

func (s Scope) team() sql.NullString {
	return sql.NullString{String: s.ID, Valid: s.Kind == "team"}
}

const usableEdgesSQL = `SELECT n.id,coalesce(n.failure_domain,'') FROM control_tunnel_nodes n
WHERE n.ready AND n.state='ready' AND n.roles @> ARRAY['edge']::text[]
AND n.transports @> ARRAY['http2','http3']::text[]
AND n.last_heartbeat_at>$1::timestamptz-interval '15 seconds' AND n.registry_expires_at>$1
AND (n.drain_deadline IS NULL OR n.drain_deadline>$1)
AND n.carrier_endpoint_host IS NOT NULL AND n.carrier_endpoint_tcp_port IS NOT NULL AND n.carrier_endpoint_quic_port IS NOT NULL
AND n.carrier_server_spki_sha256 IS NOT NULL AND n.carrier_server_certificate_chain_pem IS NOT NULL
AND paperboat.node_workspace_edge_eligible(n.id,$2,$3)
ORDER BY n.id`

const retainedEdgesSQL = `SELECT n.id FROM control_tunnel_nodes n
WHERE n.roles @> ARRAY['edge']::text[] AND n.last_heartbeat_at>$1::timestamptz-$4::interval
AND paperboat.node_workspace_edge_eligible(n.id,$2,$3)`

// ReconcileScope makes the workspace's stored edge set current: a set keeps its members while they
// stay eligible and recently seen, a gone member is replaced, and a change of mode (managed pool
// versus the workspace's own servers) starts over. It returns whether the stored set changed.
func ReconcileScope(ctx context.Context, tx *db.Tx, scope Scope, now time.Time) (bool, error) {
	if scope.ID == "" || scope.Account == "" || (scope.Kind != "account" && scope.Kind != "team") {
		return false, errors.New("edge set scope is invalid")
	}
	var mode string
	if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_edge_mode($1,$2)`, scope.Account, scope.team()).Scan(&mode); err != nil {
		return false, fmt.Errorf("read workspace edge mode: %w", err)
	}
	var currentMode string
	var current []string
	var generation int64
	err := tx.QueryRow(ctx, `SELECT mode,node_ids,generation FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2 FOR UPDATE`, scope.Kind, scope.ID).Scan(&currentMode, &current, &generation)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read workspace edge set: %w", err)
	}
	if exists && currentMode != mode {
		current = nil
	}
	usable, err := readCandidates(ctx, tx, usableEdgesSQL, now, scope)
	if err != nil {
		return false, err
	}
	retained := map[string]bool{}
	rows, err := tx.Query(ctx, retainedEdgesSQL, now, scope.Account, scope.team(), fmt.Sprintf("%d seconds", int(ReplaceAfter.Seconds())))
	if err != nil {
		return false, fmt.Errorf("read retained edges: %w", err)
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		retained[id] = true
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return false, fmt.Errorf("read retained edges: %w", err)
	}
	chosen := choose(scope.Kind+"/"+scope.ID, current, retained, usable, Size)
	if len(chosen) == 0 {
		// No edge can serve the workspace right now; keep whatever it had.
		return false, nil
	}
	if exists && currentMode == mode && equal(current, chosen) {
		return false, nil
	}
	if exists {
		_, err = tx.Exec(ctx, `UPDATE workspace_edge_sets SET mode=$3,node_ids=$4,generation=generation+1,assigned_at=$5 WHERE scope_kind=$1 AND scope_id=$2`, scope.Kind, scope.ID, mode, chosen, now)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,node_ids,assigned_at) VALUES($1,$2,$3,$4,$5)`, scope.Kind, scope.ID, mode, chosen, now)
	}
	if err != nil {
		return false, fmt.Errorf("store workspace edge set: %w", err)
	}
	return true, nil
}

func readCandidates(ctx context.Context, tx *db.Tx, query string, now time.Time, scope Scope) ([]Candidate, error) {
	rows, err := tx.Query(ctx, query, now, scope.Account, scope.team())
	if err != nil {
		return nil, fmt.Errorf("read usable edges: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.NodeID, &c.FailureDomain); err != nil {
			return nil, fmt.Errorf("read usable edges: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const scopesSQL = `SELECT kind,id,account FROM (
 SELECT 'account'::text AS kind,m.user_id AS id,m.user_id AS account FROM user_machines m JOIN users u ON u.id=m.user_id AND u.status='active'
  WHERE m.owner_team_id IS NULL AND m.deleted_at IS NULL AND m.revoked_at IS NULL GROUP BY m.user_id
 UNION ALL SELECT 'team',t.team_id,t.owner_account FROM teams t WHERE t.deleted_at IS NULL AND NOT t.suspended
) s WHERE (kind,id)>($1,$2) ORDER BY kind,id LIMIT $3`

// ReconcileAll walks every workspace once, a page per transaction, and returns how many sets changed.
func ReconcileAll(ctx context.Context, database *db.DB, now time.Time) (int, error) {
	const page = 100
	changed := 0
	kind, id := "", ""
	for {
		var scopes []Scope
		rows, err := database.Pool().Query(ctx, scopesSQL, kind, id, page)
		if err != nil {
			return changed, fmt.Errorf("list workspaces: %w", err)
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
			return changed, fmt.Errorf("list workspaces: %w", err)
		}
		for _, scope := range scopes {
			var did bool
			err = database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				var inner error
				did, inner = ReconcileScope(ctx, tx, scope, now)
				return inner
			})
			if err != nil {
				return changed, fmt.Errorf("reconcile %s %s: %w", scope.Kind, scope.ID, err)
			}
			if did {
				changed++
			}
		}
		if len(scopes) < page {
			return changed, nil
		}
		kind, id = scopes[len(scopes)-1].Kind, scopes[len(scopes)-1].ID
	}
}

// EnsureTx gives a workspace its first edge set inside the caller's transaction (a machine attaching
// before the periodic reconcile has seen its workspace). An existing set is left to the reconciler.
func EnsureTx(ctx context.Context, tx *db.Tx, scope Scope, now time.Time) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2)`, scope.Kind, scope.ID).Scan(&present); err != nil {
		return fmt.Errorf("read workspace edge set: %w", err)
	}
	if present {
		return nil
	}
	_, err := ReconcileScope(ctx, tx, scope, now)
	return err
}
