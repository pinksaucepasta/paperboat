package tunnelv1

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type healthExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// RefreshHealth projects current, exact edge-probed route readiness. A connected
// connector alone is insufficient, and a completed operation stays historical
// when a later outage makes the current health pending again. A route is ready
// only when every live edge holding its active assignment has observed it ready:
// a tunnel hostname can resolve to any of those edges (the managed wildcard), so
// "ready on one edge" sent first requests to an edge without the route (F8-06). The expiry
// reconciler retains ownership of persisting expired summary/audit state.
func RefreshHealth(ctx context.Context, executor healthExecutor, accountID string, tunnelIDs []string, now time.Time) error {
	_, err := executor.Exec(ctx, `WITH health AS (
 SELECT t.id, CASE WHEN t.desired_state='deleted' THEN 'deleted'
 WHEN t.expires_at IS NOT NULL AND t.expires_at<=$3 THEN 'expired'
 WHEN t.desired_state='paused' THEN 'paused'
 WHEN paperboat.workspace_edge_mode(t.account_id,paperboat.publication_workspace_team('tunnel',t.id))='self-hosted-only'
 AND EXISTS(SELECT 1 FROM tunnel_routes needed WHERE needed.tunnel_id=t.id AND needed.protocol IN('http','tls') AND needed.desired_state='active' AND needed.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM tunnel_domains d WHERE d.route_id=needed.id AND d.account_id=t.account_id AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear')) THEN 'custom_domain_required'
 WHEN count(r.id)>0 AND bool_and(coalesce(probe.ready,false)) AND EXISTS(SELECT 1 FROM tunnel_route_origin_health oh JOIN tunnel_routes orr ON orr.id=oh.route_id
 WHERE oh.tunnel_id=t.id AND oh.state='unreachable' AND oh.observed_at>$3::timestamptz-($7::double precision*interval '1 second')
 AND orr.desired_state='active' AND orr.deleted_at IS NULL) THEN 'origin_unreachable'
 WHEN count(r.id)>0 AND bool_and(coalesce(probe.ready,false)) THEN 'ready'
 WHEN bool_or(coalesce(probe.failed,false)) THEN 'route_unavailable'
 ELSE 'pending' END AS code
 FROM tunnels t LEFT JOIN tunnel_routes r ON r.tunnel_id=t.id AND r.desired_state='active' AND r.deleted_at IS NULL
 LEFT JOIN LATERAL (SELECT bool_and(ok) AS ready, bool_or(bad) AS failed FROM (
 SELECT a.observed_state='ready' AND a.observed_at>$3::timestamptz-interval '15 seconds' AS ok,
 a.observed_state IN ('failed','degraded') AND a.observed_at>$3::timestamptz-interval '15 seconds' AS bad
 FROM tunnel_edge_route_assignments a
 JOIN tunnel_connectors c ON c.id=a.connector_id AND c.tunnel_id=t.id AND c.host_id=a.host_id
 JOIN tunnel_connector_sessions s ON s.id=a.connector_session_id AND s.connector_id=c.id
 JOIN tunnel_config_generations g ON g.tunnel_id=t.id AND g.generation=a.config_generation
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch
 JOIN user_machines m ON m.id=c.host_id AND m.user_id=t.account_id
 JOIN control_environments environment ON environment.id=m.environment_id AND environment.owner_user_id=t.account_id AND environment.workspace_id=m.id
 JOIN machine_control_sessions identity ON identity.machine_id=m.id AND identity.installation_generation=m.installation_generation
 JOIN tunnel_connector_credential_generations credential ON credential.connector_id=c.id AND credential.tunnel_id=t.id AND credential.generation=s.credential_generation
 JOIN current_tunnel_route_viewer_policy v ON v.route_id=r.id
 WHERE a.route_id=r.id AND a.account_id=t.account_id AND a.state='active'
 AND a.route_generation=r.generation AND a.route_revision=r.generation
 AND a.access_mode=v.effective_access_mode AND a.viewer_policy_generation=v.viewer_policy_generation
 AND c.desired_state='active' AND c.revoked_at IS NULL AND c.drain_state='accepting'
 AND c.generation=a.connector_generation AND c.last_session_id=s.id
 AND c.last_applied_config_generation=a.config_generation
 AND s.process_generation=a.connector_process_generation AND s.state='ready'
 AND s.applied_config_generation=a.config_generation AND s.lease_deadline>$3
 AND s.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
 AND g.activation_state='active' AND g.content_hash=a.config_content_hash
 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.public_identity_key=a.machine_identity_public_key
 AND m.state NOT IN ('pending','revoked','deleted') AND m.seat_state='occupied'
 AND environment.desired_state='active' AND environment.revoked_at IS NULL
 AND identity.issued_at<=$3 AND identity.expires_at>$3
 AND credential.state IN ('active','overlap') AND credential.revoked_at IS NULL AND credential.valid_until>$3
 AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
 AND n.registry_expires_at>$3 AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
 AND n.drain_deadline IS NULL AND paperboat.node_route_eligible(n.id,t.account_id,r.id)
 UNION ALL
 -- A tunnel on the endpoint engine (PLAN 3a step 4): its route on each edge where the agent's
 -- runtime carrier holds it, observed ready by that edge.
 SELECT x.observed_ready AND x.observed_at>$3::timestamptz-interval '2 minutes', false
 FROM endpoint_edge_assignments x
 JOIN endpoints e ON e.id=x.endpoint_id AND e.kind='tunnel' AND e.tunnel_id=t.id AND e.desired_state='active'
 JOIN endpoint_routes er ON er.id=x.route_id AND er.tunnel_route_id=r.id
 JOIN runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id
 AND ra.edge_process_epoch=x.edge_process_epoch AND ra.session_id=x.carrier_session_id
 JOIN control_tunnel_nodes n ON n.id=x.edge_node_id AND n.process_epoch=x.edge_process_epoch
 WHERE x.desired='active' AND ra.expires_at>$3
 AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
 AND n.registry_expires_at>$3 AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
 AND n.drain_deadline IS NULL AND paperboat.node_route_eligible(n.id,t.account_id,r.id)
 ) probes) probe ON true
 WHERE t.account_id=$1 AND t.id=ANY($2::text[]) GROUP BY t.id
 ), updated AS (
 -- Edges report every few seconds; only a changed summary is written. Pending operations are settled from the
 -- projected health below, whether or not the summary changed.
 UPDATE tunnels t SET summary_code=h.code,summary_transitioned_at=$3
 FROM health h WHERE t.id=h.id AND h.code<>'expired' AND t.summary_code IS DISTINCT FROM h.code RETURNING t.id
 ), projected AS (
 SELECT t.id,h.code AS summary_code,t.desired_state FROM tunnels t JOIN health h ON h.id=t.id
 )
 UPDATE operations o SET state=CASE WHEN t.summary_code='expired' AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch') THEN 'failed' ELSE 'succeeded' END,
 phase=CASE WHEN t.summary_code='expired' AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch') THEN 'failed' ELSE 'ready' END,progress=100,retrying=false,next_retry_at=NULL,
 error_code=CASE WHEN t.summary_code='expired' AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch') THEN 'tunnel_expired' ELSE NULL END,completed_at=$3,updated_at=$3
 FROM projected t WHERE o.account_id=$1 AND o.resource_kind='tunnel' AND o.resource_id=t.id
 AND o.state IN ('pending','running') AND (
 (t.summary_code IN ('ready','expired','origin_unreachable') AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch'))
 OR (t.desired_state IN ('paused','deleted') AND o.operation_type=CASE t.desired_state WHEN 'paused' THEN $5::text ELSE $6::text END
 AND NOT EXISTS (SELECT 1 FROM tunnel_edge_route_assignments a WHERE a.tunnel_id=t.id AND a.state<>'detached'))
 )`, accountID, tunnelIDs, now, "tunnel."+stateOperationName(DesiredActive), "tunnel."+stateOperationName(DesiredPaused), "tunnel."+stateOperationName(DesiredDeleted), OriginHealthMaxAge.Seconds())
	return err
}
