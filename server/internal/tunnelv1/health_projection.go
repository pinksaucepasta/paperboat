package tunnelv1

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type healthExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// RefreshHealth projects current, exact edge-probed route readiness. A connected
// connector alone is insufficient, and a completed operation stays historical
// when a later outage makes the current health pending again. A route is ready
// when at least one live edge holding its active assignment has observed it ready
// (paperboat.endpoint_route_ready, shared with preview readiness), so one lagging edge does not
// make the route flap; that edge alone answers visitors wrongly until it converges. The expiry
// reconciler retains ownership of persisting expired summary/audit state.
func RefreshHealth(ctx context.Context, executor healthExecutor, accountID string, tunnelIDs []string, now time.Time) error {
	_, err := executor.Exec(ctx, `WITH locked AS MATERIALIZED (
 SELECT t.id,t.account_id,t.desired_state,t.expires_at FROM tunnels t
 WHERE t.account_id=$1 AND t.id=ANY($2::text[]) ORDER BY t.id FOR NO KEY UPDATE OF t
 ), health AS (
 SELECT t.id,t.desired_state,CASE WHEN t.desired_state='deleted' THEN 'deleted'
 WHEN t.expires_at IS NOT NULL AND t.expires_at<=$3 THEN 'expired'
 WHEN t.desired_state='paused' THEN 'paused'
 WHEN paperboat.workspace_edge_mode(t.account_id,paperboat.publication_workspace_team('tunnel',t.id))='self-hosted-only'
 AND EXISTS(SELECT 1 FROM tunnel_routes needed WHERE needed.tunnel_id=t.id AND needed.protocol IN('http','tls') AND needed.desired_state='active' AND needed.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM tunnel_domains d WHERE d.route_id=needed.id AND d.account_id=t.account_id AND d.deleted_at IS NULL AND EXISTS(SELECT 1 FROM paperboat.workspace_domains wd WHERE wd.id=d.workspace_domain_id AND wd.ownership_state='verified') AND d.conflict_state='clear')) THEN 'custom_domain_required'
 WHEN count(r.id)>0 AND bool_and(coalesce(probe.ready,false)) AND EXISTS(SELECT 1 FROM tunnel_route_origin_health oh JOIN tunnel_routes orr ON orr.id=oh.route_id
 WHERE oh.tunnel_id=t.id AND oh.state='unreachable' AND oh.observed_at>$3::timestamptz-($7::double precision*interval '1 second')
 AND orr.desired_state='active' AND orr.deleted_at IS NULL) THEN 'origin_unreachable'
 WHEN count(r.id)>0 AND bool_and(coalesce(probe.ready,false)) THEN 'ready'
 WHEN bool_or(coalesce(probe.failed,false)) THEN 'route_unavailable'
 ELSE 'pending' END AS code,
 -- Operations (what --wait polls) settle only once every live holder edge serves the route, so a
 -- visitor's first request on either edge of the pair is answered; the summary keeps any-holder.
 count(r.id)>0 AND bool_and(coalesce(probe.all_ready,false)) AS all_ready
 FROM locked t LEFT JOIN tunnel_routes r ON r.tunnel_id=t.id AND r.desired_state='active' AND r.deleted_at IS NULL
 LEFT JOIN LATERAL (SELECT bool_and(paperboat.endpoint_route_ready(er.id,$3,false)) AS ready,
 bool_and(paperboat.endpoint_route_ready(er.id,$3,true)) AS all_ready, false AS failed
 FROM endpoint_routes er JOIN endpoints e ON e.id=er.endpoint_id AND e.kind='tunnel' AND e.tunnel_id=t.id AND e.desired_state='active'
 WHERE er.tunnel_route_id=r.id) probe ON true
 GROUP BY t.id,t.account_id,t.desired_state,t.expires_at
 ), updated AS (
 -- Edges report every few seconds; only a changed summary is written. Pending operations are settled from the
 -- projected health below, whether or not the summary changed.
 UPDATE tunnels t SET summary_code=h.code,summary_transitioned_at=$3
 FROM health h WHERE t.id=h.id AND h.code<>'expired' AND t.summary_code IS DISTINCT FROM h.code RETURNING t.id
 ), pending AS (
 SELECT o.id,t.code AS summary_code
 FROM operations o JOIN health t ON t.id=o.resource_id WHERE o.account_id=$1 AND o.resource_kind='tunnel'
 AND o.state IN ('pending','running') AND (
 ((t.code='expired' OR (t.code IN ('ready','origin_unreachable') AND t.all_ready)
 -- An address change is done when the old name stops serving: SettleAddressMoves drops the hold, the next probe settles it.
 AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_address_holds h WHERE h.tunnel_id=t.id AND h.kind='incoming' AND h.operation_id=o.id)) AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch'))
 OR (t.desired_state IN ('paused','deleted') AND o.operation_type=CASE t.desired_state WHEN 'paused' THEN $5::text ELSE $6::text END
 AND NOT EXISTS (SELECT 1 FROM endpoint_edge_assignments x JOIN endpoints e ON e.id=x.endpoint_id WHERE e.tunnel_id=t.id AND x.desired='active')
 -- The edges have dropped the route: every live edge fetched a route snapshot that began after the change (the
 -- second absorbs the change's commit after the operation's own timestamp).
 AND NOT EXISTS (SELECT 1 FROM control_tunnel_nodes n WHERE n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
 AND n.tunnel_endpoints_epoch=n.process_epoch AND n.drain_deadline IS NULL AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
 AND (n.tunnel_snapshot_at IS NULL OR n.tunnel_snapshot_at<o.updated_at+interval '1 second')))
 )
 ORDER BY o.resource_id,o.id FOR NO KEY UPDATE OF o
 )
 UPDATE operations o SET state=CASE WHEN t.summary_code='expired' AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch') THEN 'failed' ELSE 'succeeded' END,
 phase=CASE WHEN t.summary_code='expired' AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch') THEN 'failed' ELSE 'ready' END,progress=100,retrying=false,next_retry_at=NULL,
 error_code=CASE WHEN t.summary_code='expired' AND o.operation_type IN ('tunnel.create',$4::text,'tunnel.patch') THEN 'tunnel_expired' ELSE NULL END,completed_at=$3,updated_at=$3
 FROM pending t WHERE o.id=t.id`, accountID, tunnelIDs, now, "tunnel."+stateOperationName(DesiredActive), "tunnel."+stateOperationName(DesiredPaused), "tunnel."+stateOperationName(DesiredDeleted), OriginHealthMaxAge.Seconds())
	if err != nil {
		return err
	}
	// A passthrough domain (no certificate) is added once every live edge holding its route serves the route and
	// has fetched a route snapshot begun after the binding was written, so either edge of the pair answers it.
	// shortcut: a passthrough add deleted before it completes stays running (no caller waits on it after delete).
	_, err = executor.Exec(ctx, `UPDATE operations o SET state='succeeded',phase='ready',progress=100,outcome='changed',
 result_reference=d.id,retrying=false,next_retry_at=NULL,error_code=NULL,completed_at=$3,updated_at=$3
 FROM tunnel_domains d JOIN tunnel_routes r ON r.id=d.route_id
 WHERE o.account_id=$1 AND o.resource_id=d.id AND o.resource_kind='domain_binding' AND o.operation_type='domain.create' AND o.state IN ('pending','running')
 AND d.account_id=$1 AND d.tunnel_id=ANY($2::text[]) AND d.certificate_strategy='none' AND d.deleted_at IS NULL
 AND coalesce((SELECT bool_and(er.revision=r.generation AND paperboat.endpoint_route_ready(er.id,$3,true))
  FROM endpoint_routes er WHERE er.tunnel_route_id=r.id),false)
 AND d.conflict_state='clear' AND paperboat.workspace_domain_verified(d.workspace_domain_id)
 AND NOT EXISTS (SELECT 1 FROM control_tunnel_nodes n JOIN endpoint_edge_assignments x ON x.edge_node_id=n.id AND x.edge_process_epoch=n.process_epoch
  JOIN endpoint_routes er ON er.id=x.route_id AND er.tunnel_route_id=r.id WHERE x.desired='active'
  AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
  AND n.tunnel_endpoints_epoch=n.process_epoch AND n.drain_deadline IS NULL AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
  AND (n.tunnel_snapshot_at IS NULL OR n.tunnel_snapshot_at<o.updated_at+interval '1 second'))`, accountID, tunnelIDs, now)
	return err
}

// RefreshPendingStateOperations settles the pause, deletion and passthrough domain operations still waiting for the
// edges. An edge calls it after each route snapshot fetch (that fetch is what completes them) and the certificate
// worker calls it every tick, so an add waiting through a long edge outage still completes once the edges serve it.
func RefreshPendingStateOperations(ctx context.Context, q interface {
	healthExecutor
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, now time.Time) error {
	// A passthrough add whose domain, route or tunnel went away can never be served: fail it with a clear code
	// instead of leaving `domain add --wait` running for good.
	if _, err := q.Exec(ctx, `UPDATE operations o SET state='failed',phase='failed',progress=100,retrying=false,next_retry_at=NULL,
 error_code=CASE WHEN t.expires_at<=$1 AND t.deleted_at IS NULL AND t.desired_state<>'deleted' AND d.deleted_at IS NULL
  AND r.deleted_at IS NULL AND r.desired_state<>'deleted' THEN 'tunnel_expired' ELSE 'domain_binding_removed' END,completed_at=$1,updated_at=$1
 FROM tunnel_domains d LEFT JOIN tunnel_routes r ON r.id=d.route_id LEFT JOIN tunnels t ON t.id=d.tunnel_id
 WHERE o.state IN ('pending','running') AND o.resource_kind='domain_binding' AND o.operation_type='domain.create'
 AND o.resource_id=d.id AND o.account_id=d.account_id AND d.certificate_strategy='none'
 AND (d.deleted_at IS NOT NULL OR r.id IS NULL OR r.deleted_at IS NOT NULL OR r.desired_state='deleted'
  OR t.id IS NULL OR t.deleted_at IS NOT NULL OR t.desired_state='deleted' OR t.expires_at<=$1)`, now); err != nil {
		return err
	}
	// Three batches, so none can fill another's (passthrough adds are projected through their tunnel, RefreshHealth):
	// tunnel state operations; passthrough adds of the last 15 minutes, so a new add always gets in even when older
	// ones wait (paused tunnel, offline connector); and older adds only once they can complete now (live tunnel,
	// verified clear domain, every edge serving the route), so an add that outlived a long edge outage completes
	// and adds that cannot finish yet never occupy a batch.
	rows, err := q.Query(ctx, `(SELECT account_id, resource_id AS tunnel FROM operations
 WHERE state IN ('pending','running') AND resource_kind='tunnel' AND operation_type IN ($1,$2) AND resource_id IS NOT NULL
 ORDER BY updated_at LIMIT 100)
 UNION ALL (SELECT o.account_id, d.tunnel_id FROM operations o JOIN tunnel_domains d ON d.id=o.resource_id AND d.account_id=o.account_id
 WHERE o.state IN ('pending','running') AND o.resource_kind='domain_binding' AND o.operation_type='domain.create' AND d.certificate_strategy='none' AND d.deleted_at IS NULL
 AND o.updated_at>$3::timestamptz-interval '15 minutes'
 ORDER BY o.updated_at LIMIT 100)
 UNION ALL (SELECT o.account_id, d.tunnel_id FROM operations o JOIN tunnel_domains d ON d.id=o.resource_id AND d.account_id=o.account_id
 JOIN tunnels t ON t.id=d.tunnel_id JOIN tunnel_routes r ON r.id=d.route_id
 WHERE o.state IN ('pending','running') AND o.resource_kind='domain_binding' AND o.operation_type='domain.create' AND d.certificate_strategy='none' AND d.deleted_at IS NULL
 AND o.updated_at<=$3::timestamptz-interval '15 minutes' AND t.desired_state='active' AND t.deleted_at IS NULL
 AND d.conflict_state='clear' AND paperboat.workspace_domain_verified(d.workspace_domain_id)
 AND coalesce((SELECT bool_and(er.revision=r.generation AND paperboat.endpoint_route_ready(er.id,$3,true))
  FROM endpoint_routes er WHERE er.tunnel_route_id=r.id),false)
 ORDER BY o.updated_at LIMIT 100)`,
		"tunnel."+stateOperationName(DesiredPaused), "tunnel."+stateOperationName(DesiredDeleted), now)
	if err != nil {
		return err
	}
	byAccount := map[string][]string{}
	for rows.Next() {
		var account, tunnel string
		if err := rows.Scan(&account, &tunnel); err != nil {
			rows.Close()
			return err
		}
		byAccount[account] = append(byAccount[account], tunnel)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// One account that keeps failing must not stop the others from settling.
	var errs []error
	for account, tunnels := range byAccount {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := RefreshHealth(ctx, q, account, tunnels, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
