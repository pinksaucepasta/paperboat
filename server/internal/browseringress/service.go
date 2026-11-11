package browseringress

import (
	"context"
	"database/sql"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/ingressprojection"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/privateaccess"
)

type NativeIngressAuthority interface {
	AuthorizeIngress(context.Context, privateaccess.AuthorizeInput) (privateaccess.Decision, error)
}

// Service composes current viewer authority with the current exact connector
// target. Both ingress and the machine's independent admission call this owner.
type Service struct {
	Native NativeIngressAuthority
	DB     *db.DB
	Access *browseraccess.Service
	Quota  accountingauthority.QuotaChecker
	// Visitor signs the visitor assertion of gated routes with Issuer; nil sends none.
	Visitor *mint.Provider
	Issuer  string
}

// visitorAssertionLifetime bounds the assertion an app receives; apps verify it on arrival, and a
// longer window than the decision's tolerates clock skew between Paperboat and the app.
const visitorAssertionLifetime = 5 * time.Minute

func (s *Service) Resolve(ctx context.Context, a browseraccess.Authorization, node, epoch string) (wire.IngressDecision, error) {
	var d wire.IngressDecision
	var err error
	policyGeneration := int64(0)
	if a.ResourceKind == "lazy_policy" {
		policyGeneration = a.ResourceGeneration
		var expiry time.Time
		var preview, route string
		var routeGeneration int64
		err = s.DB.Pool().QueryRow(ctx, browseraccess.LazyPreviewSQL, a.ResourceID, a.ResourceGeneration, "", "", a.IssuedAt).Scan(&preview, &route, &routeGeneration, &expiry)
		if err != nil {
			if err == pgx.ErrNoRows {
				return d, wire.ErrIngressDenied
			}
			return d, err
		}
		a.ResourceKind, a.ResourceID, a.RouteID = "preview", preview, route
		a.ResourceGeneration, a.RouteGeneration = routeGeneration, routeGeneration
		if expiry.Before(a.ExpiresAt) {
			a.ExpiresAt = expiry
		}
	}
	if a.ResourceKind == "tunnel" {
		if a.ViewerPolicyGeneration < 1 {
			return d, refused(ctx, a, "viewer_policy_missing")
		}
		policyGeneration = a.ViewerPolicyGeneration
		d, err = (ingressprojection.SQLIngressSource{DB: s.DB}).RestrictedRoute(ctx, node, epoch, a.AccessMode, a.RouteID, a.Hostname)
	} else if a.ResourceKind == "preview" {
		d, err = s.preview(ctx, a, node, epoch)
	} else {
		return d, refused(ctx, a, "resource_kind")
	}
	if err != nil {
		if err == pgx.ErrNoRows || err == wire.ErrIngressDenied {
			return d, refused(ctx, a, "no_edge_decision")
		}
		return d, err
	}
	if a.ResourceKind == "tunnel" && d.PolicyGeneration != uint64(a.ViewerPolicyGeneration) {
		return wire.IngressDecision{}, refused(ctx, a, "viewer_policy_generation")
	}
	b := &d.Binding
	switch {
	case b.AccountID != a.OwnerAccountID:
		return wire.IngressDecision{}, refused(ctx, a, "owner")
	case b.ResourceGeneration != uint64(a.ResourceGeneration):
		return wire.IngressDecision{}, refused(ctx, a, "resource_generation")
	case b.RouteID != a.RouteID, b.RouteGeneration != uint64(a.RouteGeneration):
		return wire.IngressDecision{}, refused(ctx, a, "route")
	case b.Hostname != a.Hostname:
		return wire.IngressDecision{}, refused(ctx, a, "hostname")
	}
	b.Audience = a.AccessMode
	if policyGeneration != 0 {
		d.PolicyGeneration = uint64(policyGeneration)
	}
	d.PrincipalID = a.AccountID
	d.GrantID = a.GrantID
	d.GrantGeneration = uint64(a.GrantGeneration)
	if d.GrantGeneration == 0 {
		d.GrantGeneration = 1
	}
	d.MembershipGeneration = uint64(a.MembershipGeneration)
	d.IssuedAt = a.IssuedAt
	if a.ExpiresAt.Before(d.ExpiresAt) {
		d.ExpiresAt = a.ExpiresAt
	}
	d.DecisionID = a.GrantID
	d.Action = "view"
	if err = d.Validate(time.Now().UTC()); err != nil {
		return wire.IngressDecision{}, err
	}
	if err := s.recordUsage(ctx, a, d); err != nil {
		return wire.IngressDecision{}, err
	}
	if s.Visitor != nil && a.AccessMode != "public" && a.AccountID != "" {
		var email string
		if err := s.DB.Pool().QueryRow(ctx, `SELECT primary_email FROM users WHERE id=$1`, a.AccountID).Scan(&email); err != nil && err != pgx.ErrNoRows {
			return wire.IngressDecision{}, err
		}
		now := time.Now().UTC()
		expires := now.Add(visitorAssertionLifetime)
		if a.SessionExpiresAt.After(now) && a.SessionExpiresAt.Before(expires) {
			expires = a.SessionExpiresAt
		}
		if d.VisitorAssertion, err = s.Visitor.SignVisitorAssertion(s.Issuer, a.Hostname, a.AccountID, email, a.TeamID, now, expires); err != nil {
			return wire.IngressDecision{}, err
		}
	}
	return d, nil
}

func (s *Service) recordUsage(ctx context.Context, a browseraccess.Authorization, d wire.IngressDecision) error {
	b := d.Binding
	team := a.TeamID
	if team == "" {
		if err := s.DB.SQL().QueryRowContext(ctx, `SELECT coalesce(owner_team_id,'') FROM user_machines WHERE id=$1 AND environment_id=$2 AND user_id=$3`, b.HostID, b.EnvironmentID, b.AccountID).Scan(&team); err != nil {
			if err == sql.ErrNoRows {
				return wire.ErrIngressDenied
			}
			return err
		}
	}
	actor := a.OwnerAccountID
	if team != "" {
		actor = a.AccountID
	}
	return accountingauthority.Record(ctx, s.DB, accountingauthority.Binding{
		ID: "grant:" + a.GrantID, Node: d.EdgeNodeID, Environment: b.EnvironmentID,
		Route: b.RouteID, Revision: int64(b.RouteGeneration), User: actor, Team: team,
		IssuedAt: d.IssuedAt, ExpiresAt: d.ExpiresAt,
	}, s.Quota)
}

func (s *Service) preview(ctx context.Context, a browseraccess.Authorization, node, epoch string) (wire.IngressDecision, error) {
	var d wire.IngressDecision
	b := &d.Binding
	err := s.DB.Pool().QueryRow(ctx, previewSQL, a.ResourceID, a.OwnerAccountID, a.RouteID, node, epoch, a.IssuedAt).Scan(
		&b.EnvironmentID, &b.AccountID, &b.TunnelID, &b.ResourceGeneration, &b.RouteID, &b.RouteGeneration, &b.HostID, &b.InstallationGeneration,
		&b.Hostname, &b.OriginScheme, &b.OriginAddress, &d.EdgeNodeID, &d.EdgeProcessEpoch, &d.ConnectorID, &d.SessionID, &d.ProcessGeneration, &d.ConfigGeneration, &d.AssignmentGeneration, &d.ExpiresAt)
	if err != nil {
		return d, err
	}
	if b.OriginScheme != "unix" && b.OriginScheme != "file" {
		host, port, err := net.SplitHostPort(b.OriginAddress)
		if err != nil {
			return d, wire.ErrIngressDenied
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		b.OriginAddress = net.JoinHostPort(host, port)
	}
	b.Lifecycle = wire.TunnelEphemeral
	b.ConnectionMethod = "edge"
	b.Protocol = "http"
	b.PathPrefix = "/"
	b.TargetID = b.RouteID
	b.TargetGeneration = b.ResourceGeneration
	b.PublicationID = a.ResourceID
	b.PublicationGeneration = b.ResourceGeneration
	b.TLSVerification = "not_applicable"
	if b.OriginScheme == "https" {
		b.TLSVerification = "system"
	}
	d.PolicyGeneration = b.ResourceGeneration
	return d, nil
}

// Readiness and renewals advance the immutable lease revision. An active
// attachment may retain its admitted revision, but may never claim a future one.
const previewSQL = `
SELECT m.environment_id,p.account_id,a.tunnel_id,a.route_generation,a.route_id,a.route_generation,a.host_id,m.installation_generation,
 substring(p.endpoint from 9),p.target_scheme,p.target_address,a.edge_node_id,a.edge_process_epoch,a.connector_id,a.connector_session_id,
 a.process_generation,a.config_generation,a.route_generation,
 LEAST($6::timestamptz+interval '10 seconds',a.expires_at,p.lease_deadline,p.user_deadline,identity.expires_at)
FROM preview_routes a
JOIN preview_leases p ON p.id=a.preview_id AND p.account_id=a.account_id
JOIN user_machines m ON m.id=a.host_id AND m.user_id=p.account_id
JOIN machine_control_sessions identity ON identity.machine_id=m.id AND identity.installation_generation=m.installation_generation
JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch
WHERE p.id=$1 AND p.account_id=$2 AND a.route_id=$3 AND a.edge_node_id=$4 AND a.edge_process_epoch=$5
 AND p.terminal_state='active' AND p.lease_deadline>$6 AND (p.user_deadline IS NULL OR p.user_deadline>$6)
 AND a.lease_generation>0 AND a.lease_generation<=p.generation AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>$6
 AND p.access_mode IN ('private','team')
 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.seat_state='occupied'
 AND m.state NOT IN ('pending','revoked','deleted') AND m.public_identity_key=a.machine_identity_public_key
 AND identity.issued_at<=$6 AND identity.expires_at>$6
 AND n.state='ready' AND n.ready AND n.last_heartbeat_at>$6::timestamptz-interval '2 minutes'
LIMIT 1`

// refused logs which check refused a browser ingress decision (no credentials), then answers the
// ordinary denial; the edge cannot tell the steps apart, operators can.
func refused(ctx context.Context, a browseraccess.Authorization, step string) error {
	slog.WarnContext(ctx, "browser ingress refused", "step", step, "resource_kind", a.ResourceKind, "resource_id", a.ResourceID, "route_id", a.RouteID)
	return wire.ErrIngressDenied
}
