package endpoints

import (
	"context"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// agentServesTunnels is the SQL predicate (parameter $1: the machine) for an agent whose tunnels
// ride on its runtime carriers: it holds at least one live carrier, and every live carrier
// advertises tunnel endpoints to an edge process that serves them. Until then the machine's
// tunnels keep their own connectors.
const agentServesTunnels = `EXISTS(SELECT 1 FROM runtime_carrier_attachments a WHERE a.machine_id=$1 AND a.expires_at>now())
AND NOT EXISTS(SELECT 1 FROM runtime_carrier_attachments a LEFT JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 WHERE a.machine_id=$1 AND a.expires_at>now() AND (NOT a.agent_capabilities @> ARRAY['endpoints','tunnel_endpoints']::text[]
 OR n.tunnel_endpoints_epoch IS DISTINCT FROM a.edge_process_epoch))`

// engineTunnel is the SQL predicate (over tunnels t) for a live tunnel the engine can serve now.
// Its routes are all HTTP until TCP, TLS and private TCP ride on agent carriers (3a step 4.4): a
// tunnel moves as a whole, so a tunnel with another route kind keeps its connector.
const engineTunnel = liveTunnel + ` AND t.stable_endpoint LIKE 'https://_%' AND NOT EXISTS(SELECT 1 FROM tunnel_routes o WHERE o.tunnel_id=t.id
AND o.desired_state='active' AND o.deleted_at IS NULL AND o.protocol<>'http')`

// SyncAgentTunnelsTx makes the machine's tunnel endpoints match its tunnels: while its agent serves
// tunnels, every live tunnel it created is an endpoint with one endpoint route per active route;
// otherwise it has none, and its tunnels run on their connectors. Then it reconciles the agent's
// edge assignments.
func SyncAgentTunnelsTx(ctx context.Context, tx *db.Tx, machineID string) error {
	var serves bool
	if err := tx.QueryRow(ctx, `SELECT `+agentServesTunnels, machineID).Scan(&serves); err != nil {
		return err
	}
	if !serves {
		if _, err := tx.Exec(ctx, `DELETE FROM endpoints WHERE kind='tunnel' AND agent_machine_id=$1`, machineID); err != nil {
			return err
		}
		return ReconcileAgentTx(ctx, tx, machineID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM endpoints e WHERE e.kind='tunnel' AND e.agent_machine_id=$1
AND NOT EXISTS(SELECT 1 FROM tunnels t WHERE t.id=e.tunnel_id AND t.created_by_host_id=$1 AND `+engineTunnel+`)`, machineID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO endpoints(id,kind,account_id,owner_team_id,agent_machine_id,access_mode,lifetime,tunnel_id)
SELECT t.id,'tunnel',t.account_id,t.owner_team_id,$1,t.access_mode,'persistent',t.id FROM tunnels t
WHERE t.created_by_host_id=$1 AND `+engineTunnel+`
ON CONFLICT(id) DO UPDATE SET access_mode=EXCLUDED.access_mode,owner_team_id=EXCLUDED.owner_team_id,updated_at=now()
WHERE endpoints.access_mode IS DISTINCT FROM EXCLUDED.access_mode OR endpoints.owner_team_id IS DISTINCT FROM EXCLUDED.owner_team_id`, machineID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM endpoint_routes r USING endpoints e WHERE r.endpoint_id=e.id AND e.kind='tunnel' AND e.agent_machine_id=$1
AND NOT EXISTS(SELECT 1 FROM tunnel_routes tr WHERE tr.id=r.tunnel_route_id AND tr.tunnel_id=e.tunnel_id AND tr.desired_state='active' AND tr.deleted_at IS NULL)`, machineID); err != nil {
		return err
	}
	// An engine tunnel has no connector to acknowledge its config: the agent serves the routes in
	// tunnel_routes, so the newest config generation is the active one (the DNS and status
	// projections read the active config).
	const newest = `SELECT max(h.generation) FROM tunnel_config_generations h WHERE h.tunnel_id=g.tunnel_id AND h.activation_state IN ('pending','active')`
	if _, err := tx.Exec(ctx, `UPDATE tunnel_config_generations g SET activation_state='superseded',activated_at=NULL
FROM endpoints e WHERE e.tunnel_id=g.tunnel_id AND e.kind='tunnel' AND e.agent_machine_id=$1
AND g.activation_state='active' AND g.generation<>(`+newest+`)`, machineID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tunnel_config_generations g SET activation_state='active',activated_at=now()
FROM endpoints e WHERE e.tunnel_id=g.tunnel_id AND e.kind='tunnel' AND e.agent_machine_id=$1
AND g.activation_state='pending' AND g.generation=(`+newest+`)`, machineID); err != nil {
		return err
	}
	// The route's id is the tunnel route's id, so the edge, the agent and accounting name it the same
	// way on both paths; its revision follows the route generation.
	if _, err := tx.Exec(ctx, `INSERT INTO endpoint_routes(id,endpoint_id,hostname,protocol,revision,tunnel_route_id)
SELECT tr.id,e.id,lower(substring(t.stable_endpoint from 9)),'http',tr.generation,tr.id
FROM endpoints e JOIN tunnels t ON t.id=e.tunnel_id JOIN tunnel_routes tr ON tr.tunnel_id=t.id AND tr.desired_state='active' AND tr.deleted_at IS NULL
WHERE e.kind='tunnel' AND e.agent_machine_id=$1
ON CONFLICT(id) DO UPDATE SET revision=EXCLUDED.revision WHERE endpoint_routes.revision<>EXCLUDED.revision`, machineID); err != nil {
		return err
	}
	return ReconcileAgentTx(ctx, tx, machineID)
}

// SyncTunnelAgents runs SyncAgentTunnelsTx, one transaction each, for every machine whose tunnels
// may need to move: agents advertising tunnel endpoints that created a live tunnel, and machines
// that have tunnel endpoints. It is how tunnel and route changes reach the engine.
func SyncTunnelAgents(ctx context.Context, store *db.DB, limit int) error {
	rows, err := store.Pool().Query(ctx, `SELECT machine_id FROM (
SELECT a.machine_id FROM runtime_carrier_attachments a WHERE a.expires_at>now() AND a.agent_capabilities @> ARRAY['tunnel_endpoints']::text[]
 AND EXISTS(SELECT 1 FROM tunnels t WHERE t.created_by_host_id=a.machine_id AND `+liveTunnel+`)
UNION SELECT e.agent_machine_id FROM endpoints e WHERE e.kind='tunnel') m ORDER BY machine_id LIMIT $1`, limit)
	if err != nil {
		return err
	}
	machines := []string{}
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			rows.Close()
			return err
		}
		machines = append(machines, machine)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var failures error
	for _, machine := range machines {
		// One machine's failure must not hold back the others.
		failures = errors.Join(failures, store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return SyncAgentTunnelsTx(ctx, tx, machine) }))
	}
	return failures
}

// tunnelAssignmentPrefix marks the edge assignment id of a tunnel route on the engine; the rest is
// the route id. It is what an edge reports back in its route observations.
const tunnelAssignmentPrefix = "epa_"

// IsTunnelAssignmentID reports whether an edge route assignment id names an engine tunnel route.
func IsTunnelAssignmentID(id string) bool { return strings.HasPrefix(id, tunnelAssignmentPrefix) }
