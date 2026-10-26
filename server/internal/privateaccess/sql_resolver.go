package privateaccess

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// SQLResolver is the production account-scoped resolver. Preview rows are
// read through the canonical preview attachment repository while durable
// tunnel rows, route rows, and the current connector session are checked in a
// single SQL statement. Every call rechecks current state and expiry.
type SQLResolver struct {
	db      *db.DB
	preview *PreviewAttachmentResolver
	clock   func() time.Time
}

func NewSQLResolver(database *db.DB, attachments AttachmentStore) (*SQLResolver, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: private access database is not open", ErrInvalid)
	}
	preview, err := NewPreviewAttachmentResolver(attachments)
	if err != nil {
		return nil, err
	}
	return &SQLResolver{db: database, preview: preview, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func (r *SQLResolver) SetClock(now func() time.Time) error {
	if r == nil || now == nil {
		return fmt.Errorf("%w: SQL resolver clock is required", ErrInvalid)
	}
	r.clock = now
	return r.preview.SetClock(now)
}

func (r *SQLResolver) ResolvePrivate(ctx context.Context, lookup Lookup) (Binding, error) {
	if r == nil || r.db == nil || r.db.Pool() == nil || ctx == nil {
		return Binding{}, ErrIdentityUnavailable
	}
	if err := lookup.Edge.Validate(); err != nil {
		return Binding{}, newDenied(ReasonIdentityInvalid)
	}
	if err := lookup.Request.Validate(); err != nil {
		return Binding{}, err
	}
	if lookup.AccountID == "" || lookup.AccountID != lookup.Request.AccountID {
		return Binding{}, newDenied(ReasonAccountMismatch)
	}
	if lookup.Request.ResourceKind == ResourcePreview {
		return r.preview.ResolvePrivate(ctx, lookup)
	}
	if lookup.Request.ResourceKind != ResourceTunnel {
		return Binding{}, ErrResourceNotFound
	}
	now := lookup.Now
	if now.IsZero() {
		now = r.clock().UTC()
	}
	return r.resolveTunnel(ctx, lookup, now)
}

func (r *SQLResolver) resolveTunnel(ctx context.Context, lookup Lookup, now time.Time) (Binding, error) {
	request := lookup.Request
	// A tunnel route is private only when both the durable tunnel and exact
	// route are active. The connector and session are required to be the
	// current accepting/ready pair, so a stale reconnect cannot authorize.
	row := r.db.Pool().QueryRow(ctx, resolvePrivateTunnelSQL,
		lookup.AccountID, request.ResourceID, request.RouteID, request.ConnectorID,
		request.CarrierSessionID, now, now.Add(MaximumDecisionTTL), lookup.Edge.NodeID, lookup.Edge.ProcessEpoch,
	)
	var (
		accountID, tunnelID, routeID, connectorID, sessionID                                                      string
		protocol, accessMode, tunnelState, routeState, connectorState, drainState, sessionState                   string
		generation, routeGeneration, sessionGeneration, processGeneration, configGeneration, assignmentGeneration int64
		expiresAt                                                                                                 time.Time
		hostname, pathPrefix                                                                                      string
		edgeNodeID, edgeProcessEpoch                                                                              string
	)
	if err := row.Scan(&accountID, &tunnelID, &routeID, &connectorID, &sessionID, &generation, &routeGeneration, &sessionGeneration, &processGeneration, &configGeneration, &assignmentGeneration, &protocol, &accessMode, &tunnelState, &routeState, &connectorState, &drainState, &sessionState, &expiresAt, &hostname, &pathPrefix, &edgeNodeID, &edgeProcessEpoch); err != nil {
		if pureExpectedPrivateAccessError(err, pgx.ErrNoRows) {
			return Binding{}, ErrResourceNotFound
		}
		return Binding{}, fmt.Errorf("%w: resolve private tunnel route: %w", ErrRouteUnavailable, err)
	}
	if accountID != lookup.AccountID || tunnelID != request.ResourceID || routeID != request.RouteID || connectorID != request.ConnectorID || sessionID != request.CarrierSessionID {
		return Binding{}, ErrResourceNotFound
	}
	if tunnelState != "active" || routeState != "active" || connectorState != "active" || drainState != "accepting" || sessionState != "ready" {
		return Binding{}, ErrRouteUnavailable
	}
	if accessMode != "private" {
		return Binding{}, newDenied(ReasonProtocolDenied)
	}
	if generation < 1 || routeGeneration < 1 || sessionGeneration < 1 || processGeneration < 1 || configGeneration < 1 || assignmentGeneration < 1 || !expiresAt.After(now) {
		return Binding{}, newDenied(ReasonExpired)
	}
	if protocol == "private_tcp" {
		protocol = ProtocolTCP
	} else if protocol == "http" {
		protocol = ProtocolHTTP
	} else {
		return Binding{}, newDenied(ReasonProtocolDenied)
	}
	if strings.HasPrefix(hostname, "https://") {
		hostname = hostFromEndpoint(hostname)
	}
	return Binding{
		AccountID: accountID, ResourceKind: ResourceTunnel, ResourceID: tunnelID, RouteID: routeID,
		ConnectorID: connectorID, CarrierSessionID: sessionID, RouteGeneration: uint64(routeGeneration),
		SessionGeneration: uint64(sessionGeneration), ProcessGeneration: uint64(processGeneration), ConfigGeneration: uint64(configGeneration), AssignmentGeneration: uint64(assignmentGeneration), Protocol: protocol,
		AccessMode: accessMode, State: "ready", ExpiresAt: expiresAt, Hostname: hostname, PathPrefix: pathPrefix,
		EdgeNodeID: edgeNodeID, EdgeProcessEpoch: edgeProcessEpoch,
	}, nil
}

// resolvePrivateTunnelSQL reads a private tunnel route served on the endpoint engine (PLAN 3a): the
// "connector" and session are the agent's runtime carrier on that edge, as the edge's stream carries.
const resolvePrivateTunnelSQL = `
SELECT t.account_id, t.id, r.id, (ra.admission->'binding'->>'connector_id'), ra.session_id,
       t.generation, r.generation, 1::bigint, ra.worker_generation,
       (ra.admission->'binding'->>'config_generation')::bigint, x.generation, r.protocol, v.effective_access_mode,
       t.desired_state, r.desired_state, 'active', 'accepting', 'ready',
       LEAST(COALESCE(t.expires_at, $7), ra.expires_at, $7),
       COALESCE(r.match_hostname, t.stable_endpoint), COALESCE(r.path_prefix, ''),
       x.edge_node_id, x.edge_process_epoch
FROM endpoint_edge_assignments AS x
JOIN endpoints AS e ON e.id = x.endpoint_id AND e.kind = 'tunnel' AND e.desired_state = 'active'
JOIN tunnels AS t ON t.id = e.tunnel_id
JOIN endpoint_routes AS er ON er.id = x.route_id
JOIN tunnel_routes AS r ON r.id = er.tunnel_route_id AND r.tunnel_id = t.id
JOIN current_tunnel_route_viewer_policy AS v ON v.route_id = r.id
JOIN runtime_carrier_attachments AS ra
  ON ra.machine_id = e.agent_machine_id AND ra.edge_node_id = x.edge_node_id
 AND ra.edge_process_epoch = x.edge_process_epoch AND ra.session_id = x.carrier_session_id
JOIN control_tunnel_nodes AS edge_node
  ON edge_node.id = x.edge_node_id AND edge_node.process_epoch = x.edge_process_epoch
JOIN user_machines AS machine ON machine.id = e.agent_machine_id AND machine.user_id = t.account_id
WHERE t.account_id = $1
  AND t.id = $2
  AND r.id = $3
  AND ra.admission->'binding'->>'connector_id' = $4
  AND ra.session_id = $5
  AND x.edge_node_id = $8
  AND x.edge_process_epoch = $9
  AND x.desired = 'active' AND x.observed_ready
  AND v.effective_access_mode = 'private'
  AND t.deleted_at IS NULL
  AND r.deleted_at IS NULL
  AND ra.expires_at > $6
	AND machine.state = 'online'
	AND machine.online
	AND machine.deleted_at IS NULL
	AND machine.revoked_at IS NULL
	AND edge_node.state = 'ready'
	AND edge_node.ready = true
	AND edge_node.last_heartbeat_at IS NOT NULL
	AND edge_node.last_heartbeat_at > $6::timestamptz - interval '2 minutes'
  AND (t.expires_at IS NULL OR t.expires_at > $6)
FOR SHARE OF t, r`
