package ingressprojection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const maximumIngressDecisions = 4096

// SQLIngressSource projects current publication authority and retains its minimal
// accounting binding for delayed signed usage.
// Existing tunnel tables represent durable resources. Ephemeral creation and
// public TCP allocation will supply their own persisted lifecycle fields.
type SQLIngressSource struct {
	DB    *db.DB
	Clock wire.Clock
	Quota accountingauthority.QuotaChecker
	// QuotaDenied, when set, receives each public route left out of a projection solely
	// because its workspace is out of bandwidth allowance (never an authority).
	QuotaDenied *[]wire.IngressQuotaDenial
	// edge marks an edge's complete snapshot (Edge): there one route whose decision cannot be
	// issued is withheld and named in the log instead of failing every route on that edge.
	edge bool
}

// rowFailed handles one route's failure inside a projection. In an edge snapshot a route whose
// own data cannot yield a decision (ErrIngressDenied, ErrInvalidInput, an accounting binding that
// cannot be retained) is withheld, which denies that route alone, and the cause is named. Any other
// failure, and every failure outside an edge snapshot, fails the projection.
func (s SQLIngressSource) rowFailed(ctx context.Context, stage, routeID string, err error) error {
	if !s.edge {
		return err
	}
	if !(errors.Is(err, wire.ErrIngressDenied) || errors.Is(err, wire.ErrInvalidInput) || errors.Is(err, accountingauthority.ErrUnavailable) || errors.Is(err, sql.ErrNoRows)) {
		slog.WarnContext(ctx, "ingress snapshot failed", "component", "paperboat-server", "operation", "edge_ingress", "stage", stage, "route_id", routeID, "cause", err.Error())
		return err
	}
	slog.WarnContext(ctx, "ingress decision withheld", "component", "paperboat-server", "operation", "edge_ingress", "stage", stage, "route_id", routeID, "cause", err.Error())
	return nil
}

func (s SQLIngressSource) Edge(ctx context.Context, nodeID, epoch string) ([]wire.IngressDecision, error) {
	if wire.ValidateIdentifier(nodeID) != nil || wire.ValidateOpaqueEpoch(epoch) != nil {
		return nil, wire.ErrInvalidInput
	}
	s.edge = true
	durable, err := s.project(ctx, nodeID, epoch, "public", "", "")
	if err != nil {
		return nil, err
	}
	previews, err := s.publicPreviews(ctx, nodeID, epoch)
	if err != nil {
		return nil, err
	}
	if len(durable)+len(previews) > maximumIngressDecisions {
		return nil, wire.ErrIngressDenied
	}
	return append(durable, previews...), nil
}

// engineTunnelIngressSQL projects the public (or, for a browser grant, the restricted) authority of
// tunnel routes served on the endpoint engine (PLAN 3a step 4) for one edge process: the route
// rides on the agent's runtime carrier, so the decision's connector, session, generations and
// tunnel id are that carrier's identity, which is what the edge's stream preface carries. The route
// id names the tunnel route. Parameters: now, edge node, edge epoch, audience, route, hostname.
const engineTunnelIngressSQL = `
WITH valid_carrier AS MATERIALIZED (
 -- runtime_carrier_valid and the account's edge eligibility run once per carrier and per account on this edge, not
 -- once per route (~1 ms per call each: 1,000 routes took 2 s).
 SELECT a.route_id FROM runtime_carrier_attachments a
 WHERE a.edge_node_id = $2 AND a.edge_process_epoch = $3
   AND paperboat.runtime_carrier_valid(a.route_id, a.edge_node_id, a.edge_process_epoch, false)
), managed_account AS MATERIALIZED (
 SELECT DISTINCT t.account_id, t.owner_team_id FROM endpoint_edge_assignments x
 JOIN endpoints e ON e.id = x.endpoint_id JOIN tunnels t ON t.id = e.tunnel_id
 WHERE x.edge_node_id = $2 AND x.edge_process_epoch = $3 AND x.desired = 'active'
   AND paperboat.workspace_edge_mode(t.account_id, t.owner_team_id) = 'managed-only'
   AND paperboat.node_workspace_edge_eligible($2, t.account_id, t.owner_team_id)
)
SELECT t.account_id,environment.id,(ra.admission->'binding'->>'tunnel_id'),t.generation,r.id,r.generation,ra.machine_id,m.installation_generation,
 publication.hostname,CASE WHEN r.protocol IN ('tcp','tls') THEN '' ELSE COALESCE(r.path_prefix,'/') END,r.origin_scheme,r.origin_address,
 r.tls_verification,COALESCE(r.tls_server_name,''),COALESCE(r.tls_ca,''),COALESCE(r.mtls_credential_reference,''),COALESCE(r.tls_pin,''),
 COALESCE(r.public_tcp_listener_id,''),COALESCE(r.public_tcp_port,0),r.protocol,
 publication.publication_id,publication.publication_generation,
 x.edge_node_id,x.edge_process_epoch,(c.admission->'binding'->>'connector_id'),c.session_id,c.process_generation,
 (c.admission->'binding'->>'config_generation')::bigint,c.generation,viewer_policy.viewer_policy_generation,
 LEAST($1::timestamptz + interval '10 seconds',t.expires_at,identity.expires_at,c.expires_at,node.registry_expires_at,node.last_heartbeat_at+interval '15 seconds')
FROM endpoint_edge_assignments x
JOIN endpoints e ON e.id=x.endpoint_id AND e.kind='tunnel' AND e.desired_state='active'
JOIN tunnels t ON t.id=e.tunnel_id
JOIN endpoint_routes er ON er.id=x.route_id
JOIN tunnel_routes r ON r.id=er.tunnel_route_id AND r.tunnel_id=t.id
-- A lateral lookup by route id, fenced (OFFSET 0): under this query's written join order a plain join to the view
-- recomputed it over every route in the database for each row.
CROSS JOIN LATERAL (SELECT v.effective_access_mode,v.viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy v WHERE v.route_id=r.id OFFSET 0) viewer_policy
LEFT JOIN LATERAL (
 SELECT COALESCE(r.match_hostname,substring(t.stable_endpoint from 9)) AS hostname,r.id AS publication_id,r.generation AS publication_generation
 WHERE paperboat.workspace_edge_mode(t.account_id,t.owner_team_id)='managed-only' OR r.protocol='tcp'
 OR (r.match_type='exact' AND EXISTS(SELECT 1 FROM tunnel_domains exact_domain WHERE exact_domain.account_id=t.account_id AND exact_domain.tunnel_id=t.id AND exact_domain.route_id=r.id AND exact_domain.hostname=r.match_hostname AND exact_domain.deleted_at IS NULL AND exact_domain.ownership_state='verified' AND exact_domain.conflict_state='clear' AND exact_domain.match_type='exact' AND (r.protocol='http' OR(exact_domain.certificate_strategy='none' AND exact_domain.certificate_state='not_applicable'))))
 UNION ALL
 SELECT d.hostname,d.id,d.generation FROM tunnel_domains d
 WHERE r.protocol IN ('http','tls') AND d.account_id=t.account_id
   AND d.route_id=r.id AND d.tunnel_id=r.tunnel_id AND d.deleted_at IS NULL
   AND d.ownership_state='verified' AND d.conflict_state='clear' AND d.match_type='exact'
   AND d.hostname<>COALESCE(r.match_hostname,substring(t.stable_endpoint from 9))
   AND (r.protocol='http' OR (d.certificate_strategy='none' AND d.certificate_state='not_applicable'))
) publication ON true
JOIN runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id
 AND ra.edge_process_epoch=x.edge_process_epoch
 AND ra.agent_capabilities @> ARRAY['endpoints','tunnel_endpoints']::text[]
-- The assignment's carrier, and during a worker handover's bounded overlap the carrier it replaced: the edge
-- keeps the route there until the new carrier serves it (migrations 227, 228; preview_ingress.go).
CROSS JOIN LATERAL (
 SELECT ra.admission,ra.session_id,ra.worker_generation AS process_generation,x.generation,ra.expires_at WHERE ra.session_id=x.carrier_session_id
 UNION ALL
 SELECT ra.previous_admission,x.previous_carrier_session_id,(ra.previous_admission->'binding'->>'process_generation')::bigint,
  x.previous_generation,ra.previous_until
 WHERE ra.previous_until>$1 AND ra.previous_admission->'binding'->>'session_id'=x.previous_carrier_session_id
) c
JOIN user_machines m ON m.id=ra.machine_id AND m.user_id=t.account_id
JOIN control_environments environment ON environment.id=m.environment_id AND environment.owner_user_id=t.account_id AND environment.workspace_id=m.id
JOIN machine_control_sessions identity ON identity.machine_id=m.id AND identity.installation_generation=m.installation_generation
JOIN control_tunnel_nodes node ON node.id=x.edge_node_id AND node.process_epoch=x.edge_process_epoch
WHERE x.edge_node_id=$2 AND x.edge_process_epoch=$3 AND x.desired='active' AND node.tunnel_endpoints_epoch=x.edge_process_epoch
 AND ra.route_id IN (SELECT route_id FROM valid_carrier)
 AND (t.owner_team_id IS NULL OR paperboat.workspace_tunnel_management_allowed(t.account_id,t.owner_team_id,t.id))
 AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$1)
 AND environment.desired_state='active' AND environment.revoked_at IS NULL
 AND viewer_policy.effective_access_mode=$4
 AND ($5::text='' OR r.id=$5)
 AND ($6::text='' OR publication.hostname=$6)
 AND r.desired_state='active' AND r.deleted_at IS NULL AND r.protocol IN ('http','tcp','tls')
 AND (r.protocol<>'tls' OR r.match_hostname=substring(t.stable_endpoint from 9))
 AND (r.match_type IN ('managed','exact') OR ($4<>'public' AND r.match_type='catch_all'))
 AND ((r.protocol='http' AND r.origin_scheme IN ('http','https','h2c')) OR (r.protocol IN ('tcp','tls') AND r.origin_scheme='tcp'))
 AND r.tls_verification IN ('not_applicable','system','custom_ca','insecure','pinned')
 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.state NOT IN ('pending','revoked','deleted')
 AND m.seat_state='occupied' AND m.public_identity_key=ra.public_identity_key
 AND identity.issued_at<=$1 AND identity.expires_at>$1
 AND node.registry_expires_at>$1 AND node.roles @> ARRAY['edge']::text[] AND node.drain_deadline IS NULL
 -- A managed-only account eligible on this edge with a tunnel placed on no self-hosted node is node_route_eligible by
 -- its definition; anything else takes the full check.
 AND ((EXISTS(SELECT 1 FROM managed_account ma WHERE ma.account_id=t.account_id AND ma.owner_team_id IS NOT DISTINCT FROM t.owner_team_id)
   AND NOT EXISTS(SELECT 1 FROM selfhost_placements sp WHERE sp.resource_kind='tunnel' AND sp.resource_id=t.id))
  OR paperboat.node_route_eligible(node.id,t.account_id,r.id))
 AND node.state='ready' AND node.ready=true AND node.last_heartbeat_at>$1::timestamptz-interval '15 seconds'
ORDER BY x.route_id,publication.hostname,publication.publication_id
LIMIT 4097`

// queryInWrittenJoinOrder runs one ingress projection in a read-only transaction that keeps the
// query's written join order. The projections join about fifteen tables, views and inlined
// functions; the planner's join search took 4.5 s per edge pull under load (prod 2026-10-23,
// 13 ms to execute), so edges timed out and every pull answered 503. Both projections are written
// starting from the edge's assignments, which is also the best plan. finish closes the rows and
// ends the transaction.
func queryInWrittenJoinOrder(ctx context.Context, database *sql.DB, query string, args ...any) (*sql.Rows, func() error, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	for _, setting := range []string{`SET LOCAL join_collapse_limit=1`, `SET LOCAL from_collapse_limit=1`} {
		if _, err = tx.ExecContext(ctx, setting); err != nil {
			return nil, nil, errors.Join(err, tx.Rollback())
		}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, errors.Join(err, tx.Rollback())
	}
	return rows, func() error {
		closeErr := rows.Close()
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(closeErr, rollbackErr)
		}
		return closeErr
	}, nil
}

func (s SQLIngressSource) project(ctx context.Context, nodeID, epoch, audience, routeID, hostname string) (decisions []wire.IngressDecision, resultErr error) {
	if ctx == nil || s.DB == nil || s.DB.SQL() == nil {
		return nil, wire.ErrInvalidInput
	}
	now := time.Now().UTC()
	if s.Clock != nil {
		now = s.Clock.Now().UTC()
	}
	out, err := s.scanIngressRows(ctx, now, engineTunnelIngressSQL, now, nodeID, epoch, audience, routeID, hostname)
	if err != nil {
		return nil, err
	}
	if audience == "public" {
		admitted := out[:0]
		for _, d := range out {
			b := d.Binding
			var team string
			var entitled bool
			if err = s.DB.SQL().QueryRowContext(ctx, `SELECT coalesce(t.owner_team_id,m.owner_team_id,''),coalesce(t.owner_team_id,m.owner_team_id) IS NULL OR paperboat.team_paid_entitled(coalesce(t.owner_team_id,m.owner_team_id)) FROM user_machines m JOIN tunnel_routes r ON r.id=$4 JOIN tunnels t ON t.id=r.tunnel_id WHERE m.id=$1 AND m.environment_id=$2 AND m.user_id=$3`, b.HostID, b.EnvironmentID, b.AccountID, b.RouteID).Scan(&team, &entitled); err != nil {
				if err = s.rowFailed(ctx, "entitlement", b.RouteID, err); err != nil {
					return nil, err
				}
				continue
			}
			if !entitled {
				continue
			}
			if err = accountingauthority.Record(ctx, s.DB, accountingauthority.Binding{ID: fmt.Sprintf("public:%s:%d", b.PublicationID, b.PublicationGeneration), Node: d.EdgeNodeID, Environment: b.EnvironmentID, Route: b.RouteID, Revision: int64(b.RouteGeneration), User: b.AccountID, Team: team, IssuedAt: d.IssuedAt, ExpiresAt: d.ExpiresAt}, s.Quota); err != nil {
				if resetsAt, exhausted := quotaExhaustion(err); exhausted {
					if s.QuotaDenied != nil {
						*s.QuotaDenied = append(*s.QuotaDenied, wire.IngressQuotaDenial{RouteID: b.RouteID, Hostname: b.Hostname, ResetsAt: resetsAt})
					}
					continue
				}
				if err = s.rowFailed(ctx, "accounting", b.RouteID, err); err != nil {
					return nil, err
				}
				continue
			}
			admitted = append(admitted, d)
		}
		out = admitted
	}
	return out, nil
}

// scanIngressRows runs one ingress projection query and completes each row into a decision.
func (s SQLIngressSource) scanIngressRows(ctx context.Context, now time.Time, query string, args ...any) (decisions []wire.IngressDecision, resultErr error) {
	rows, finish, err := queryInWrittenJoinOrder(ctx, s.DB.SQL(), query, args...)
	if err != nil {
		return nil, err
	}
	rowsClosed := false
	closeRows := func() error {
		if rowsClosed {
			return nil
		}
		rowsClosed = true
		return finish()
	}
	defer func() {
		if closeErr := closeRows(); closeErr != nil {
			decisions = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("close connector ingress rows: %w", closeErr))
		}
	}()
	out := make([]wire.IngressDecision, 0)
	for rows.Next() {
		if len(out) == maximumIngressDecisions {
			return nil, wire.ErrIngressDenied
		}
		var d wire.IngressDecision
		var routeCA string
		b := &d.Binding
		if err = rows.Scan(&b.AccountID, &b.EnvironmentID, &b.TunnelID, &b.ResourceGeneration, &b.RouteID, &b.RouteGeneration, &b.HostID, &b.InstallationGeneration, &b.Hostname, &b.PathPrefix, &b.OriginScheme, &b.OriginAddress, &b.TLSVerification, &b.TLSServerName, &routeCA, &b.MTLSCredentialReference, &b.TLSPin, &b.ListenerID, &b.PublicPort, &b.Protocol, &b.PublicationID, &b.PublicationGeneration, &d.EdgeNodeID, &d.EdgeProcessEpoch, &d.ConnectorID, &d.SessionID, &d.ProcessGeneration, &d.ConfigGeneration, &d.AssignmentGeneration, &d.PolicyGeneration, &d.ExpiresAt); err != nil {
			return nil, err
		}
		b.TLSCASHA256 = wire.OriginCAFingerprint(routeCA) // the agent checks the CA it has against the one authorized
		if err = completeIngressProjection(&d, now); err != nil {
			if err = s.rowFailed(ctx, "projection", d.Binding.RouteID, err); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = closeRows(); err != nil {
		return nil, fmt.Errorf("close connector ingress rows: %w", err)
	}
	return out, nil
}

// quotaExhaustion reports an exact quota answer (never one joined with another failure) and the
// allowance's reset time when the answer carries it.
func quotaExhaustion(err error) (time.Time, bool) {
	if quota, ok := err.(*accountingauthority.QuotaExhaustedError); ok {
		return quota.ResetsAt, true
	}
	return time.Time{}, err == accountingauthority.ErrQuotaExhausted
}

func completeIngressProjection(d *wire.IngressDecision, now time.Time) error {
	b := &d.Binding
	// Route owns its exact target and owner publication in the existing schema;
	// these aliases preserve generation fencing without inventing another owner.
	b.TargetID = b.RouteID
	b.TargetGeneration = b.RouteGeneration
	if b.PublicationID == "" {
		b.PublicationID = b.RouteID
	}
	if b.PublicationGeneration == 0 {
		b.PublicationGeneration = b.RouteGeneration
	}
	b.Lifecycle = wire.TunnelDurable
	b.Audience = "public"
	b.ConnectionMethod = "edge"
	host, port, err := net.SplitHostPort(b.OriginAddress)
	if err != nil {
		return wire.ErrIngressDenied
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return wire.ErrIngressDenied
	}
	b.OriginAddress = net.JoinHostPort(ip.String(), port)
	d.Action = "view"
	d.IssuedAt = now
	id, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	d.DecisionID = "decision_" + id.String()
	return d.Validate(now)
}

// RestrictedRoute returns the exact current connector/target projection for
// the hostname already authorized by the browser grant.
// The caller must compose it with independently checked browser grant authority;
// this method never issues a viewer grant or trusts a claimed principal.
func (s SQLIngressSource) RestrictedRoute(ctx context.Context, nodeID, epoch, audience, routeID, hostname string) (wire.IngressDecision, error) {
	if wire.ValidateIdentifier(nodeID) != nil || wire.ValidateOpaqueEpoch(epoch) != nil || wire.ValidateIdentifier(routeID) != nil || (hostname == "" || len(hostname) > 253) || (audience != "private" && audience != "team") {
		return wire.IngressDecision{}, wire.ErrIngressDenied
	}
	rows, err := s.project(ctx, nodeID, epoch, audience, routeID, hostname)
	if err != nil {
		return wire.IngressDecision{}, err
	}
	if len(rows) != 1 {
		return wire.IngressDecision{}, wire.ErrIngressDenied
	}
	rows[0].Binding.Audience = audience
	return rows[0], nil
}
