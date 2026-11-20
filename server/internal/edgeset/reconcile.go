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

// usableRegionsSQL lists the regions in which some edge can serve the workspace right now.
var usableRegionsSQL = `SELECT DISTINCT n.region FROM control_tunnel_nodes n
WHERE ` + usableEdge("$1") + `
AND paperboat.node_workspace_edge_candidate(n.id,$2,$3)
ORDER BY n.region`

// retainedRegionsSQL lists the regions with an eligible edge seen within $4: a region keeps its place in a
// workspace's pair while any of its nodes does.
const retainedRegionsSQL = `SELECT DISTINCT n.region FROM control_tunnel_nodes n
WHERE n.roles @> ARRAY['edge']::text[] AND n.last_heartbeat_at>$1::timestamptz-$4::interval
AND paperboat.node_workspace_edge_candidate(n.id,$2,$3)`

// ReconcileScope makes the workspace's stored pair of regions current: a pair keeps its regions while one of
// their nodes stays eligible and recently seen, a gone region is replaced, and a change of mode (managed pool
// versus the workspace's own servers) starts over. It returns whether the stored pair changed. rtt is the
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
	err := tx.QueryRow(ctx, `SELECT mode,regions,generation FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2 FOR UPDATE`, scope.Kind, scope.ID).Scan(&currentMode, &current, &generation)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read workspace edge set: %w", err)
	}
	if exists && currentMode != mode {
		current = nil
	}
	usable, err := readRegions(ctx, tx, usableRegionsSQL, now, scope)
	if err != nil {
		return false, err
	}
	retained := map[string]bool{}
	rows, err := tx.Query(ctx, retainedRegionsSQL, now, scope.Account, scope.team(), fmt.Sprintf("%d seconds", int(ReplaceAfter.Seconds())))
	if err != nil {
		return false, fmt.Errorf("read retained regions: %w", err)
	}
	for rows.Next() {
		var region string
		if err = rows.Scan(&region); err != nil {
			break
		}
		retained[region] = true
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return false, fmt.Errorf("read retained regions: %w", err)
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
// each machine's best RTT to any edge of each region (one stored sample per machine and edge), so one outlier
// machine cannot decide placement. An even count takes the lower middle value.
const latencySQL = `SELECT region,percentile_disc(0.5) WITHIN GROUP (ORDER BY best) FROM (
 SELECT n.region,l.machine_id,min(l.rtt_ms) AS best FROM machine_node_latency l JOIN user_machines m ON m.id=l.machine_id
 JOIN control_tunnel_nodes n ON n.id=l.node_id AND n.region IS NOT NULL
 WHERE l.role='edge' AND m.deleted_at IS NULL AND m.revoked_at IS NULL
  AND CASE WHEN $1='team' THEN m.owner_team_id=$2 ELSE m.user_id=$2 AND m.owner_team_id IS NULL END
  AND l.measured_at>$3::timestamptz-make_interval(secs=>$4::float8)
 GROUP BY n.region,l.machine_id) per_machine
GROUP BY region`

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
		var region string
		var rtt int
		if err := rows.Scan(&region, &rtt); err != nil {
			return nil, fmt.Errorf("read edge latency: %w", err)
		}
		out[region] = rtt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read edge latency: %w", err)
	}
	return out, nil
}

// Store writes the workspace's pair of regions, in order (the first is the primary, the second the backup): a
// new row, or the next generation of the existing one.
func Store(ctx context.Context, tx *db.Tx, scope Scope, mode string, regions []string, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,regions,assigned_at) VALUES($1,$2,$3,$4,$5)
ON CONFLICT (scope_kind,scope_id) DO UPDATE SET mode=excluded.mode,regions=excluded.regions,generation=workspace_edge_sets.generation+1,assigned_at=excluded.assigned_at`, scope.Kind, scope.ID, mode, regions, now); err != nil {
		return fmt.Errorf("store workspace edge pair: %w", err)
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

func readRegions(ctx context.Context, tx *db.Tx, query string, now time.Time, scope Scope) ([]string, error) {
	rows, err := tx.Query(ctx, query, now, scope.Account, scope.team())
	if err != nil {
		return nil, fmt.Errorf("read usable regions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			return nil, fmt.Errorf("read usable regions: %w", err)
		}
		out = append(out, region)
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

// scopesSQL lists, after ($1,$2), the workspaces whose stored pair ReconcileScope would change: no pair yet or a mode
// that moved (and an edge that can serve it exists: with none, ReconcileScope keeps whatever it had), a region with no
// node retained (silent past $6), more regions than Size, or a free place with a usable edge in another region to fill it.
// Every other workspace keeps its pair (choose keeps retained regions), so it costs nothing: walking every workspace
// each second was ~7 queries per workspace and grew with accounts, not machines.
//
// full adds the per-workspace eligibility of nodes (node_workspace_edge_candidate, ~1 ms per call and node). The quick
// form leaves it out: a region that stopped being eligible without going silent, or a self-hosted workspace's free
// place, is found by the next full pass. Mode is read from the pool policy row, not through workspace_edge_mode.
func scopesSQL(full bool) string {
	eligible := func(node string) string {
		if !full {
			return ""
		}
		return " AND paperboat.node_workspace_edge_candidate(" + node + ",s.account,s.team)"
	}
	managedOnly := ""
	if !full {
		managedOnly = " AND coalesce(pool.mode,'managed-only')='managed-only'"
	}
	return `SELECT s.kind,s.id,s.account FROM (
 SELECT 'account'::text AS kind,m.user_id AS id,m.user_id AS account,NULL::text AS team FROM user_machines m JOIN users u ON u.id=m.user_id AND u.status='active'
  WHERE m.owner_team_id IS NULL AND m.deleted_at IS NULL AND m.revoked_at IS NULL GROUP BY m.user_id
 UNION ALL SELECT 'team',t.team_id,t.owner_account,t.team_id FROM teams t WHERE t.deleted_at IS NULL AND NOT t.suspended
) s LEFT JOIN workspace_edge_sets e ON e.scope_kind=s.kind AND e.scope_id=s.id
LEFT JOIN selfhost_pool_policies pool ON pool.capability='tunnel' AND pool.scope_kind=s.kind AND pool.scope_id=s.id
WHERE (s.kind,s.id)>($1,$2) AND (
 ((e.scope_id IS NULL OR e.mode IS DISTINCT FROM coalesce(pool.mode,'managed-only'))
  AND EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE ` + usableEdge("$5") + `)` + func() string {
		if !full {
			return ")"
		}
		return `
  AND EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE ` + usableEdge("$5") + eligible("n.id") + `))`
	}() + `
 OR cardinality(e.regions)>$4
 OR EXISTS (SELECT 1 FROM unnest(e.regions) AS member(region) WHERE NOT EXISTS (
  SELECT 1 FROM control_tunnel_nodes n WHERE n.region=member.region AND n.roles @> ARRAY['edge']::text[]
   AND n.last_heartbeat_at>$5::timestamptz-$6::interval` + eligible("n.id") + `))
 OR (cardinality(e.regions)<$4` + managedOnly + ` AND EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE ` + usableEdge("$5") + `
   AND n.region<>ALL(e.regions)` + eligible("n.id") + `)))
ORDER BY s.kind,s.id LIMIT $3`
}

var quickScopesSQL, fullScopesSQL = scopesSQL(false), scopesSQL(true)

// SweepEvery is how often the worker looks for workspaces whose set must change. Explicit changes (a self-hosted
// pool edit, a machine's first attachment) reconcile or assign synchronously; the sweep is the safety net for a
// member silent past ReplaceAfter (10 minutes), so seconds are enough.
const SweepEvery = 15 * time.Second

// FullSweepEvery is how many sweeps pass between two full ones (every five minutes at SweepEvery).
const FullSweepEvery = 20

// ReconcileAll walks every workspace once with the full test, a page per transaction, and returns how many sets
// changed.
func ReconcileAll(ctx context.Context, database *db.DB, now time.Time) (int, error) {
	return Sweep(ctx, database, now, true)
}

// Sweep reconciles the workspaces whose set would change: with full false only by the quick test (SweepEvery), with
// full true by every test (the worker's every FullSweepEvery-th run, and ReconcileAll).
func Sweep(ctx context.Context, database *db.DB, now time.Time, full bool) (int, error) {
	const page = 100
	query := quickScopesSQL
	if full {
		query = fullScopesSQL
	}
	changed := 0
	kind, id := "", ""
	for {
		var scopes []Scope
		rows, err := database.Pool().Query(ctx, query, kind, id, page, Size, now, fmt.Sprintf("%d seconds", int(ReplaceAfter.Seconds())))
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
