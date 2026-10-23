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

	"github.com/pinksaucepasta/paperboat-server/internal/db"
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
	return ReconcileAgentTx(ctx, tx, p.AgentMachineID)
}

// liveEndpoint is the SQL predicate for an endpoint that should be served now. A preview is live
// while its lease is active and unexpired, and only while it does not run on its own per-preview
// carrier: an agent that kept a preview on the old path (its edges did not all serve endpoints
// yet) must not also get an endpoint route for the same hostname on an edge.
const liveEndpoint = `e.desired_state='active' AND (e.kind<>'preview' OR (EXISTS(SELECT 1 FROM preview_leases p
WHERE p.id=e.preview_lease_id AND p.terminal_state='active' AND p.lease_deadline>now())
AND NOT EXISTS(SELECT 1 FROM preview_lease_carrier_attachments la WHERE la.preview_id=e.preview_lease_id
AND la.account_id=e.account_id AND la.state NOT IN ('failed','released'))))
AND (e.kind<>'tunnel' OR EXISTS(SELECT 1 FROM tunnels t WHERE t.id=e.tunnel_id AND ` + liveTunnel + `))`

// liveTunnel is the SQL predicate (over tunnels t) for a tunnel that should be served now.
const liveTunnel = `t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())`

// ReconcileAgentTx makes the agent's assignments match its live endpoints times the runtime
// carriers it holds that can serve endpoints. A changed carrier session or edge epoch fences the
// assignment (new generation, not ready until the edge observes it again).
func ReconcileAgentTx(ctx context.Context, tx *db.Tx, machineID string) error {
	_, err := tx.Exec(ctx, `WITH desired AS (
SELECT DISTINCT ON (r.id,a.edge_node_id) r.id route_id,e.id endpoint_id,a.edge_node_id,a.edge_process_epoch,a.session_id
FROM endpoints e JOIN endpoint_routes r ON r.endpoint_id=e.id
JOIN runtime_carrier_attachments a ON a.machine_id=e.agent_machine_id AND a.agent_capabilities @> CASE WHEN e.kind='tunnel'
 THEN ARRAY['endpoints','tunnel_endpoints'] ELSE ARRAY['endpoints'] END::text[]
WHERE e.agent_machine_id=$1 AND `+liveEndpoint+`
ORDER BY r.id,a.edge_node_id,a.active DESC
), upserted AS (
INSERT INTO endpoint_edge_assignments AS x(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id)
SELECT route_id,endpoint_id,edge_node_id,edge_process_epoch,session_id FROM desired
ON CONFLICT(route_id,edge_node_id) DO UPDATE SET
 generation=CASE WHEN x.desired='active' AND x.edge_process_epoch=EXCLUDED.edge_process_epoch AND x.carrier_session_id=EXCLUDED.carrier_session_id THEN x.generation ELSE x.generation+1 END,
 observed_ready=x.observed_ready AND x.desired='active' AND x.edge_process_epoch=EXCLUDED.edge_process_epoch AND x.carrier_session_id=EXCLUDED.carrier_session_id,
 edge_process_epoch=EXCLUDED.edge_process_epoch,carrier_session_id=EXCLUDED.carrier_session_id,desired='active'
RETURNING x.route_id,x.edge_node_id
)
UPDATE endpoint_edge_assignments x SET desired='removed',observed_ready=false
FROM endpoints e WHERE e.id=x.endpoint_id AND e.agent_machine_id=$1 AND x.desired='active'
AND NOT EXISTS(SELECT 1 FROM upserted u WHERE u.route_id=x.route_id AND u.edge_node_id=x.edge_node_id)`, machineID)
	return err
}
