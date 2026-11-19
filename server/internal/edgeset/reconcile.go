package edgeset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
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

// usableEdge is the part of "an edge that may serve a workspace right now" that does not depend on the workspace; now
// is the SQL placeholder holding the observation time. The workspace's own eligibility is added by the caller.
func usableEdge(now string) string {
	return `n.ready AND n.state='ready' AND n.roles @> ARRAY['edge']::text[]
AND n.transports @> ARRAY['http2','http3']::text[]
AND n.last_heartbeat_at>` + now + `::timestamptz-interval '15 seconds' AND n.registry_expires_at>` + now + `
AND (n.drain_deadline IS NULL OR n.drain_deadline>` + now + `)
AND n.carrier_endpoint_host IS NOT NULL AND n.carrier_endpoint_tcp_port IS NOT NULL AND n.carrier_endpoint_quic_port IS NOT NULL
AND n.carrier_server_spki_sha256 IS NOT NULL AND n.carrier_server_certificate_chain_pem IS NOT NULL
AND n.public_ingress_verified_at IS NOT NULL`
}

var usableEdgesSQL = `SELECT n.id,coalesce(n.failure_domain,'') FROM control_tunnel_nodes n
WHERE ` + usableEdge("$1") + `
AND paperboat.node_workspace_edge_eligible(n.id,$2,$3)
ORDER BY n.id`

const retainedEdgesSQL = `SELECT n.id FROM control_tunnel_nodes n
WHERE n.roles @> ARRAY['edge']::text[] AND n.last_heartbeat_at>$1::timestamptz-$4::interval
AND paperboat.node_workspace_edge_eligible(n.id,$2,$3)`

// ReconcileScope makes the workspace's stored edge set current: a set keeps its members while they
// stay eligible and recently seen, a gone member is replaced, and a change of mode (managed pool
// versus the workspace's own servers) starts over. It returns whether the stored set changed. rtt is the
// latency ranking from ReadLatency, read before the transaction: its SIREAD locks over every machine's
// samples would otherwise exhaust Postgres's predicate-lock memory and fail writes to those machines
// (a revoke). The ranking is advisory, so a stale or empty one only changes which edges fill free places.
func ReconcileScope(ctx context.Context, tx *db.Tx, scope Scope, rtt map[string]int, now time.Time) (bool, error) {
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
	chosen := choose(scope.Kind+"/"+scope.ID, current, retained, rtt, usable, Size)
	if len(chosen) == 0 {
		// No edge can serve the workspace right now; keep whatever it had.
		return false, nil
	}
	if exists && currentMode == mode && equal(current, chosen) {
		return false, nil
	}
	if err = Store(ctx, tx, scope, mode, chosen, now); err != nil {
		return false, err
	}
	return true, nil
}

// latencySQL is the median, over the workspace's live machines with a sample within LatencyFresh, of
// each machine's RTT to each edge (one stored sample per machine and edge), so one outlier machine cannot
// decide placement. An even count takes the lower middle value.
const latencySQL = `SELECT l.node_id,percentile_disc(0.5) WITHIN GROUP (ORDER BY l.rtt_ms) FROM machine_node_latency l JOIN user_machines m ON m.id=l.machine_id
WHERE l.role='edge' AND m.deleted_at IS NULL AND m.revoked_at IS NULL
 AND CASE WHEN $1='team' THEN m.owner_team_id=$2 ELSE m.user_id=$2 AND m.owner_team_id IS NULL END
 AND l.measured_at>$3::timestamptz-make_interval(secs=>$4::float8)
GROUP BY l.node_id`

// ReadLatency reads the ranking outside the reconcile transaction (a plain read on the pool or any
// read-committed handle), never inside the serializable one.
func ReadLatency(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, scope Scope, now time.Time) (map[string]int, error) {
	rows, err := q.Query(ctx, latencySQL, scope.Kind, scope.ID, now, LatencyFresh.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read edge latency: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var rtt int
		if err := rows.Scan(&id, &rtt); err != nil {
			return nil, fmt.Errorf("read edge latency: %w", err)
		}
		out[id] = rtt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read edge latency: %w", err)
	}
	return out, nil
}

// Store writes the workspace's edge set, in order (the first is the primary, the second the fallback): a
// new row, or the next generation of the existing one.
func Store(ctx context.Context, tx *db.Tx, scope Scope, mode string, nodeIDs []string, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,node_ids,assigned_at) VALUES($1,$2,$3,$4,$5)
ON CONFLICT (scope_kind,scope_id) DO UPDATE SET mode=excluded.mode,node_ids=excluded.node_ids,generation=workspace_edge_sets.generation+1,assigned_at=excluded.assigned_at`, scope.Kind, scope.ID, mode, nodeIDs, now); err != nil {
		return fmt.Errorf("store workspace edge set: %w", err)
	}
	return nil
}

// NodeStatusSQL is the status an edge node shows account-visible inventories: ready only on a fresh
// heartbeat with a complete carrier endpoint, as an observation and not a promise of admission. now is
// the SQL placeholder holding the observation time; the node's alias is n.
func NodeStatusSQL(now string) string {
	return `CASE WHEN n.state='draining' OR n.drain_deadline IS NOT NULL THEN 'draining'
 WHEN n.state='ready' AND n.ready=true AND n.last_heartbeat_at>` + now + `::timestamptz-interval '15 seconds'
 AND n.registry_expires_at>` + now + `::timestamptz AND n.transports @> ARRAY['http3','http2']::text[]
 AND n.carrier_endpoint_host IS NOT NULL AND n.carrier_endpoint_tcp_port IS NOT NULL AND n.carrier_endpoint_quic_port IS NOT NULL
 AND n.carrier_server_spki_sha256 IS NOT NULL AND n.carrier_server_certificate_chain_pem IS NOT NULL
 THEN 'ready' ELSE 'unavailable' END`
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

// scopesSQL lists, after ($1,$2), the workspaces whose stored set ReconcileScope would change: no set yet or a mode
// that moved (and an edge that can serve it exists: with none, ReconcileScope keeps whatever it had), a member no longer
// retained (silent past $6, eligible or not), more members than Size, or a free place with a usable edge outside the set to
// fill it. Every other workspace keeps its set (choose keeps retained members), so it costs nothing: walking every
// workspace each second was ~7 queries per workspace and grew with accounts, not machines.
var scopesSQL = `SELECT s.kind,s.id,s.account FROM (
 SELECT 'account'::text AS kind,m.user_id AS id,m.user_id AS account,NULL::text AS team FROM user_machines m JOIN users u ON u.id=m.user_id AND u.status='active'
  WHERE m.owner_team_id IS NULL AND m.deleted_at IS NULL AND m.revoked_at IS NULL GROUP BY m.user_id
 UNION ALL SELECT 'team',t.team_id,t.owner_account,t.team_id FROM teams t WHERE t.deleted_at IS NULL AND NOT t.suspended
) s LEFT JOIN workspace_edge_sets e ON e.scope_kind=s.kind AND e.scope_id=s.id
WHERE (s.kind,s.id)>($1,$2) AND (
 ((e.scope_id IS NULL OR e.mode IS DISTINCT FROM paperboat.workspace_edge_mode(s.account,s.team))
  AND EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE ` + usableEdge("$5") + `)
  AND EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE ` + usableEdge("$5") + ` AND paperboat.node_workspace_edge_eligible(n.id,s.account,s.team)))
 OR cardinality(e.node_ids)>$4
 OR EXISTS (SELECT 1 FROM unnest(e.node_ids) AS member(id) WHERE NOT EXISTS (
  SELECT 1 FROM control_tunnel_nodes n WHERE n.id=member.id AND n.roles @> ARRAY['edge']::text[]
   AND n.last_heartbeat_at>$5::timestamptz-$6::interval AND paperboat.node_workspace_edge_eligible(n.id,s.account,s.team)))
 OR (cardinality(e.node_ids)<$4 AND EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE ` + usableEdge("$5") + `
   AND n.id<>ALL(e.node_ids) AND paperboat.node_workspace_edge_eligible(n.id,s.account,s.team))))
ORDER BY s.kind,s.id LIMIT $3`

// SweepEvery is how often the worker looks for workspaces whose set must change. Explicit changes (a self-hosted
// pool edit, a machine's first attachment) reconcile or assign synchronously; the sweep is the safety net for a
// member silent past ReplaceAfter (10 minutes), so seconds are enough.
const SweepEvery = 15 * time.Second

// ReconcileAll walks every workspace once, a page per transaction, and returns how many sets changed.
func ReconcileAll(ctx context.Context, database *db.DB, now time.Time) (int, error) {
	const page = 100
	changed := 0
	kind, id := "", ""
	for {
		var scopes []Scope
		rows, err := database.Pool().Query(ctx, scopesSQL, kind, id, page, Size, now, fmt.Sprintf("%d seconds", int(ReplaceAfter.Seconds())))
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
			rtt, err := ReadLatency(ctx, database.Pool(), scope, now)
			if err != nil {
				return changed, fmt.Errorf("reconcile %s %s: %w", scope.Kind, scope.ID, err)
			}
			err = database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				var inner error
				did, inner = ReconcileScope(ctx, tx, scope, rtt, now)
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

// LockWorkspace takes the lock every writer of a workspace's edge policy holds (the pool policy in selfhost
// takes the same key), until the transaction ends.
func LockWorkspace(ctx context.Context, tx *db.Tx, scope Scope) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('browser-edge-policy:'||$1||':'||$2,0))`, scope.Kind, scope.ID)
	return err
}

// EnsureTx gives a workspace its first edge set inside the caller's transaction (a machine attaching
// before the periodic reconcile has seen its workspace). An existing set is left to the reconciler. The first
// assignment holds the workspace lock and looks again, so it never overwrites a pair chosen while it waited.
func EnsureTx(ctx context.Context, tx *db.Tx, scope Scope, now time.Time) error {
	present := func() (bool, error) {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2)`, scope.Kind, scope.ID).Scan(&found); err != nil {
			return false, fmt.Errorf("read workspace edge set: %w", err)
		}
		return found, nil
	}
	if found, err := present(); err != nil || found {
		return err
	}
	if err := LockWorkspace(ctx, tx, scope); err != nil {
		return fmt.Errorf("lock workspace edge policy: %w", err)
	}
	if found, err := present(); err != nil || found {
		return err
	}
	// The first assignment has no latency samples to rank by (the machine is only attaching).
	_, err := ReconcileScope(ctx, tx, scope, nil, now)
	return err
}
