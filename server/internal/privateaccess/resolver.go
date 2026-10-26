package privateaccess

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// PreviewRouteResolver binds a private preview to its current route on an edge, read from
// preview_routes (the agent's runtime carrier route of the preview).
type PreviewRouteResolver struct {
	db  *db.DB
	now func() time.Time
}

func NewPreviewRouteResolver(database *db.DB) (*PreviewRouteResolver, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: preview route database is required", ErrInvalid)
	}
	return &PreviewRouteResolver{db: database, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (r *PreviewRouteResolver) SetClock(now func() time.Time) error {
	if r == nil || now == nil {
		return fmt.Errorf("%w: preview route clock is required", ErrInvalid)
	}
	r.now = now
	return nil
}

const resolvePrivatePreviewSQL = `
SELECT pr.account_id, pr.preview_id, pr.operation_id, pr.route_id, pr.connector_session_id, pr.owner_machine_id, pr.owner_session_id,
       pr.route_generation, pr.lease_generation, pr.process_generation, pr.config_generation, p.access_mode, p.endpoint,
       pr.edge_node_id, pr.edge_process_epoch, pr.state, pr.expires_at
FROM preview_routes pr JOIN preview_leases p ON p.id = pr.preview_id AND p.account_id = pr.account_id
WHERE pr.account_id = $1 AND pr.preview_id = $2 AND pr.operation_id = $3`

func (r *PreviewRouteResolver) ResolvePrivate(ctx context.Context, lookup Lookup) (Binding, error) {
	if r == nil || r.db == nil || ctx == nil {
		return Binding{}, ErrIdentityUnavailable
	}
	if lookup.Request.ResourceKind != ResourcePreview || lookup.Request.OperationID == "" {
		return Binding{}, ErrResourceNotFound
	}
	var b Binding
	var sessionState, endpoint string
	var routeGeneration, leaseGeneration, processGeneration, configGeneration int64
	err := r.db.Pool().QueryRow(ctx, resolvePrivatePreviewSQL, lookup.AccountID, lookup.Request.ResourceID, lookup.Request.OperationID).Scan(
		&b.AccountID, &b.ResourceID, &b.OperationID, &b.RouteID, &b.CarrierSessionID, &b.OwnerMachineID, &b.OwnerSessionID,
		&routeGeneration, &leaseGeneration, &processGeneration, &configGeneration, &b.AccessMode, &endpoint,
		&b.EdgeNodeID, &b.EdgeProcessEpoch, &sessionState, &b.ExpiresAt)
	if pureExpectedPrivateAccessError(err, pgx.ErrNoRows) {
		return Binding{}, ErrResourceNotFound
	}
	if err != nil {
		return Binding{}, fmt.Errorf("%w: resolve private preview route: %w", ErrRouteUnavailable, err)
	}
	b.State = "paused"
	switch sessionState {
	case "edge_ready":
		b.State = "edge_ready"
	case "ready":
		b.State = "ready"
	case "failed", "released":
		return Binding{}, newDenied(ReasonExpired)
	}
	now := lookup.Now
	if now.IsZero() {
		now = r.now().UTC()
	}
	if !b.ExpiresAt.After(now) {
		return Binding{}, newDenied(ReasonExpired)
	}
	b.ResourceKind, b.Protocol, b.Hostname = ResourcePreview, ProtocolHTTP, hostFromEndpoint(endpoint)
	b.RouteGeneration, b.SessionGeneration, b.ProcessGeneration, b.ConfigGeneration, b.AssignmentGeneration =
		uint64(routeGeneration), uint64(leaseGeneration), uint64(processGeneration), uint64(configGeneration), uint64(leaseGeneration)
	return b, nil
}
