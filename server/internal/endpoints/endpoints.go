// Package endpoints owns the shared endpoint engine (PLAN 3a): previews and tunnels are endpoints
// whose routes are served by their agent (the enrolled machine's daemon) over the runtime carriers
// that agent already holds to each edge of its workspace's edge set. An endpoint never opens a
// carrier: the reconciler assigns its routes to every edge where the agent's carrier can serve
// endpoint streams, the edge pulls those assignments with its runtime snapshot and reports what it
// observes.
package endpoints

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/keyedlock"
)

var ErrInvalid = errors.New("invalid endpoint")

// Preview is the facade input written when a preview lease is created, in the same transaction.
type Preview struct {
	LeaseID        string
	AccountID      string
	OwnerTeamID    string
	AgentMachineID string
	AccessMode     string
	Endpoint       string // https://<hostname>
}

// RegisterPreviewTx records a preview lease as a preview endpoint with its one http route and
// reconciles the agent's assignments. The lease itself stays in preview_leases.
func RegisterPreviewTx(ctx context.Context, tx *db.Tx, p Preview) error {
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" || (u.Path != "" && u.Path != "/") {
		return ErrInvalid
	}
	team := any(nil)
	if p.OwnerTeamID != "" {
		team = p.OwnerTeamID
	}
	_, err = tx.Exec(ctx, `INSERT INTO endpoints(id,kind,account_id,owner_team_id,agent_machine_id,access_mode,lifetime,preview_lease_id)
VALUES($1,'preview',$2,$3,$4,$5,'lease',$1)`, p.LeaseID, p.AccountID, team, p.AgentMachineID, p.AccessMode)
	if err != nil {
		return err
	}
	// A preview's one route is named by the preview id, so the agent maps its streams without
	// asking the server.
	_, err = tx.Exec(ctx, `INSERT INTO endpoint_routes(id,endpoint_id,hostname,protocol) VALUES($1,$1,$2,'http')`,
		p.LeaseID, strings.ToLower(u.Hostname()))
	if err != nil {
		return err
	}
	return ReconcileEndpointTx(ctx, tx, p.AgentMachineID, p.LeaseID)
}

// liveEndpoint is the SQL predicate for an endpoint that should be served now. A preview is live
// while its lease is active and unexpired.
const liveEndpoint = `e.desired_state='active' AND (e.kind<>'preview' OR EXISTS(SELECT 1 FROM preview_leases p
WHERE p.id=e.preview_lease_id AND ` + livePreview + `))
AND (e.kind<>'tunnel' OR EXISTS(SELECT 1 FROM tunnels t WHERE t.id=e.tunnel_id AND ` + liveTunnel + `))`

const livePreview = `p.terminal_state='active' AND p.lease_deadline>now()`

// LivePreviewsByOwner lists the ids of one machine's ($1) live previews. Stopped leases outnumber live ones
// (3,224 of 3,224 on prod's busiest machine), so it must stay on preview_leases_active_by_owner.
const LivePreviewsByOwner = `SELECT p.id FROM preview_leases p WHERE p.owner_machine_id=$1 AND ` + livePreview

// liveTunnel is the SQL predicate (over tunnels t) for a tunnel that should be served now.
const liveTunnel = `t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())`

// ReconcileAgentTx makes the agent's assignments match its live endpoints times the runtime
// carriers it holds that can serve endpoints. A changed carrier session or edge epoch fences the
// assignment (new generation, not ready until the edge observes it again).
func ReconcileAgentTx(ctx context.Context, tx *db.Tx, machineID string) error {
	return reconcileTx(ctx, tx, machineID, "")
}

// ReconcileEndpointTx is ReconcileAgentTx for one endpoint of the agent: what a new or changed preview or tunnel needs.
// Reconciling every live endpoint of the machine for each create read all N leases and tunnels the daemon renews
// and the edge observes, so creates cost O(N) and cancelled each other under SERIALIZABLE (F21-168: 12% of preview
// creates failed at ~400 live previews). Previews and tunnels use this one statement.
func ReconcileEndpointTx(ctx context.Context, tx *db.Tx, machineID, endpointID string) error {
	if endpointID == "" {
		return ErrInvalid
	}
	return reconcileTx(ctx, tx, machineID, endpointID)
}

// machineWriters queues local writes to one machine's carriers and routes before they take a database
// connection. The corresponding machine row lock orders the writes across server processes. Prior art: Headscale funnels node writes through one writer
// (hscontrol/state/node_store.go NodeStore.writeQueue); NetBird locks the rows it writes (store.LockingStrengthUpdate).
var machineWriters keyedlock.Locks

// LockMachine waits, until ctx ends, for the right to write machineID's carrier and route rows, and returns its release.
// Take it before the transaction begins.
func LockMachine(ctx context.Context, machineID string) (func(), error) {
	return machineWriters.Lock(ctx, machineID)
}

func reconcileTx(ctx context.Context, tx *db.Tx, machineID, endpointID string) error {
	// Callers that write a machine's routes hold LockMachine (runtime attach, preview create, tunnel changes), so its
	// carrier report and a reconcile queue instead of aborting each other with 40001 (prod 2026-11-20: 18 in 30 min).
	// Read live resources by owner before joining their projections. Starting with all of a machine's
	// endpoints looked up every preview retained for delayed accounting, even when none could be served.
	liveEndpoints := `WITH live_previews AS MATERIALIZED (` + LivePreviewsByOwner + `)
SELECT e.id,e.kind FROM live_previews p JOIN endpoints e ON e.preview_lease_id=p.id
WHERE e.agent_machine_id=$1 AND e.desired_state='active'
UNION ALL
SELECT e.id,e.kind FROM tunnels t JOIN endpoints e ON e.tunnel_id=t.id
WHERE t.created_by_host_id=$1 AND ` + liveTunnel + `
AND e.agent_machine_id=$1 AND e.desired_state='active'`
	if endpointID != "" {
		liveEndpoints = `SELECT e.id,e.kind FROM endpoints e WHERE e.agent_machine_id=$1 AND e.id=$2 AND ` + liveEndpoint
	}
	_, err := tx.Exec(ctx, `WITH live_endpoints AS (`+liveEndpoints+`), desired AS (
SELECT DISTINCT ON (r.id,a.edge_node_id) r.id route_id,e.id endpoint_id,a.edge_node_id,a.edge_process_epoch,a.session_id
FROM live_endpoints e JOIN endpoint_routes r ON r.endpoint_id=e.id
JOIN runtime_carrier_attachments a ON a.machine_id=$1 AND a.agent_capabilities @> CASE WHEN e.kind='tunnel'
 THEN ARRAY['endpoints','tunnel_endpoints'] ELSE ARRAY['endpoints'] END::text[]
JOIN user_machines um ON um.id=a.machine_id AND paperboat.node_edge_eligible_core(a.edge_node_id,um.user_id,um.owner_team_id)
ORDER BY r.id,a.edge_node_id,a.active DESC
), upserted AS (
INSERT INTO endpoint_edge_assignments AS x(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id)
SELECT route_id,endpoint_id,edge_node_id,edge_process_epoch,session_id FROM desired ORDER BY route_id,edge_node_id
ON CONFLICT(route_id,edge_node_id) DO UPDATE SET
 generation=x.generation+1,observed_ready=false,
 -- A handover to another carrier on the same edge keeps the old one for the overlap (migration 228).
 previous_carrier_session_id=CASE WHEN x.desired='active' AND x.edge_process_epoch=EXCLUDED.edge_process_epoch AND x.carrier_session_id<>EXCLUDED.carrier_session_id THEN x.carrier_session_id ELSE x.previous_carrier_session_id END,
 previous_generation=CASE WHEN x.desired='active' AND x.edge_process_epoch=EXCLUDED.edge_process_epoch AND x.carrier_session_id<>EXCLUDED.carrier_session_id THEN x.generation ELSE x.previous_generation END,
 edge_process_epoch=EXCLUDED.edge_process_epoch,carrier_session_id=EXCLUDED.carrier_session_id,desired='active'
 -- Every attach and tunnel sync reconciles; an assignment that already matches is left untouched.
 WHERE x.desired<>'active' OR x.edge_process_epoch<>EXCLUDED.edge_process_epoch OR x.carrier_session_id<>EXCLUDED.carrier_session_id
RETURNING x.route_id
), removed AS (
SELECT x.route_id,x.edge_node_id FROM endpoint_edge_assignments x JOIN endpoints e ON e.id=x.endpoint_id
WHERE e.agent_machine_id=$1 AND ($2='' OR e.id=$2) AND x.desired='active'
AND NOT EXISTS(SELECT 1 FROM desired d WHERE d.route_id=x.route_id AND d.edge_node_id=x.edge_node_id)
ORDER BY x.route_id,x.edge_node_id FOR NO KEY UPDATE OF x
)
UPDATE endpoint_edge_assignments x SET desired='removed',observed_ready=false
FROM removed r WHERE x.route_id=r.route_id AND x.edge_node_id=r.edge_node_id`, machineID, endpointID)
	return err
}
