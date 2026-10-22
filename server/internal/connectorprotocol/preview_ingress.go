package connectorprotocol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
)

// Public preview authority comes from the admitted owner publication, not a
// browser viewer grant. Its accounting binding reuses the same retained ledger
// as durable public routes and restricted browser ingress.
const publicPreviewIngressSQL = `
SELECT m.environment_id,p.account_id,a.tunnel_id,a.route_generation,a.route_id,a.route_generation,a.host_id,m.installation_generation,
 publication.hostname,p.target_scheme,p.target_address,a.edge_node_id,a.edge_process_epoch,a.connector_id,a.connector_session_id,
 a.process_generation,a.config_generation,a.route_generation,p.id,coalesce(p.owner_team_id,''),
 LEAST($3::timestamptz+interval '10 seconds',a.expires_at,p.lease_deadline,p.user_deadline,identity.expires_at,n.registry_expires_at,n.last_heartbeat_at+interval '15 seconds')
FROM preview_routes a
JOIN preview_leases p ON p.id=a.preview_id AND p.account_id=a.account_id
JOIN users owner ON owner.id=p.account_id AND owner.status='active'
JOIN user_machines m ON m.id=a.host_id AND m.id=p.owner_machine_id AND m.user_id=p.account_id
JOIN control_environments env ON env.id=m.environment_id AND env.owner_user_id=p.account_id AND env.desired_state='active' AND env.revoked_at IS NULL
JOIN machine_control_sessions identity ON identity.machine_id=m.id AND identity.installation_generation=m.installation_generation
JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch
JOIN LATERAL (
 SELECT substring(p.endpoint from 9) hostname WHERE paperboat.workspace_edge_mode(p.account_id,p.owner_team_id)='managed-only'
 UNION ALL
 SELECT d.hostname FROM preview_domains d WHERE d.preview_id=p.id AND d.account_id=p.account_id
 AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear'
 AND d.certificate_state='ready' AND d.certificate_reference IS NOT NULL AND d.preview_generation=p.generation
) publication ON true
WHERE a.edge_node_id=$1 AND a.edge_process_epoch=$2
 AND p.access_mode=$4 AND ($5::text='' OR p.id=$5) AND ($6::text='' OR a.route_id=$6) AND ($7::text='' OR publication.hostname=$7) AND ($8::text='' OR a.operation_id=$8) AND ($9::bigint=0 OR a.lease_generation=$9) AND p.terminal_state='active' AND p.lease_deadline>$3 AND (p.user_deadline IS NULL OR p.user_deadline>$3)
 AND a.lease_generation>0 AND a.lease_generation<=p.generation AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>$3
 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.seat_state='occupied' AND m.state NOT IN ('pending','revoked','deleted')
 AND m.public_identity_key=a.machine_identity_public_key AND identity.issued_at<=$3 AND identity.expires_at>$3
 AND (p.owner_team_id IS NULL OR (paperboat.team_paid_entitled(p.owner_team_id) AND paperboat.workspace_preview_management_allowed(p.account_id,p.owner_team_id,p.id)))
 AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[] AND n.registry_expires_at>$3
 AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds' AND n.drain_deadline IS NULL
 AND paperboat.node_preview_eligible(n.id,p.account_id,p.id)
 ORDER BY p.id,publication.hostname LIMIT 4097`

func (s SQLIngressSource) publicPreviews(ctx context.Context, node, epoch string) ([]wire.IngressDecision, error) {
	return s.projectPreviews(ctx, node, epoch, nil)
}

// RestrictedPreview is called only after independently verifying the native grant.
func (s SQLIngressSource) RestrictedPreview(ctx context.Context, node, epoch string, evidence wire.PrivateAccessOpen) (wire.IngressDecision, error) {
	if evidence.Validate(time.Time{}) != nil || evidence.Request.ResourceKind != "preview" || evidence.Request.Protocol != "http" || evidence.Request.AssignmentGeneration != evidence.Request.SessionGeneration {
		return wire.IngressDecision{}, wire.ErrIngressDenied
	}
	out, err := s.projectPreviews(ctx, node, epoch, &evidence)
	if err != nil {
		return wire.IngressDecision{}, err
	}
	if len(out) != 1 {
		return wire.IngressDecision{}, wire.ErrIngressDenied
	}
	return out[0], nil
}

func (s SQLIngressSource) projectPreviews(ctx context.Context, node, epoch string, evidence *wire.PrivateAccessOpen) (decisions []wire.IngressDecision, resultErr error) {
	now := time.Now().UTC()
	if s.Clock != nil {
		now = s.Clock.Now().UTC()
	}
	audience, publication, route, host, operation := "public", "", "", "", ""
	var lease uint64
	if evidence != nil {
		audience = "private"
		publication = evidence.Request.ResourceID
		route = evidence.Request.RouteID
		host = evidence.Request.Host
		operation = evidence.Request.OperationID
		lease = evidence.Request.SessionGeneration
	}
	rows, finish, err := queryInWrittenJoinOrder(ctx, s.DB.SQL(), publicPreviewIngressSQL, node, epoch, now, audience, publication, route, host, operation, lease)
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
			resultErr = errors.Join(resultErr, fmt.Errorf("close preview ingress rows: %w", closeErr))
		}
	}()
	out := []wire.IngressDecision{}
	teams := []string{}
	for rows.Next() {
		if len(out) >= maximumIngressDecisions {
			return nil, wire.ErrIngressDenied
		}
		var d wire.IngressDecision
		var team string
		b := &d.Binding
		if err := rows.Scan(&b.EnvironmentID, &b.AccountID, &b.TunnelID, &b.ResourceGeneration, &b.RouteID, &b.RouteGeneration, &b.HostID, &b.InstallationGeneration, &b.Hostname, &b.OriginScheme, &b.OriginAddress, &d.EdgeNodeID, &d.EdgeProcessEpoch, &d.ConnectorID, &d.SessionID, &d.ProcessGeneration, &d.ConfigGeneration, &d.AssignmentGeneration, &b.PublicationID, &team, &d.ExpiresAt); err != nil {
			return nil, err
		}
		b.Lifecycle = wire.TunnelEphemeral
		b.Audience = audience
		b.ConnectionMethod = "edge"
		b.Protocol = "http"
		b.PathPrefix = "/"
		b.TargetID = b.RouteID
		b.TargetGeneration = b.ResourceGeneration
		b.PublicationGeneration = b.ResourceGeneration
		b.TLSVerification = "not_applicable"
		if b.OriginScheme == "https" {
			b.TLSVerification = "system"
		}
		if b.OriginScheme != "unix" {
			host, port, err := net.SplitHostPort(b.OriginAddress)
			if err != nil {
				if err = s.rowFailed(ctx, "preview_origin", b.RouteID, wire.ErrIngressDenied); err != nil {
					return nil, err
				}
				continue
			}
			if host == "localhost" {
				host = "127.0.0.1"
			}
			b.OriginAddress = net.JoinHostPort(host, port)
		}
		d.PolicyGeneration = b.ResourceGeneration
		d.Action = "view"
		d.IssuedAt = now
		d.DecisionID = "decision_" + uuid.NewString()
		if evidence != nil {
			q := evidence.Request
			d.NativeAuthorization = evidence
			d.AssignmentGeneration = q.AssignmentGeneration
			d.PrincipalID = q.MachineID
			d.GrantID = q.Nonce
			d.GrantGeneration = q.InstallationGeneration
			if q.ExpiresAt.Before(d.ExpiresAt) {
				d.ExpiresAt = q.ExpiresAt
			}
		}
		if err := d.Validate(now); err != nil {
			if err = s.rowFailed(ctx, "preview_projection", b.RouteID, err); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, d)
		teams = append(teams, team)
	}
	rowsErr := rows.Err()
	closeErr := closeRows()
	if closeErr != nil {
		closeErr = fmt.Errorf("close preview ingress rows: %w", closeErr)
	}
	if rowsErr != nil || closeErr != nil {
		return nil, errors.Join(rowsErr, closeErr)
	}
	admitted := out[:0]
	for i, d := range out {
		b := d.Binding
		authority := fmt.Sprintf("public:%s:%d", b.PublicationID, b.PublicationGeneration)
		if evidence != nil {
			authority = "grant:" + evidence.Request.Nonce
		}
		err := accountingauthority.Record(ctx, s.DB, accountingauthority.Binding{ID: authority, Node: d.EdgeNodeID, Environment: b.EnvironmentID, Route: b.RouteID, Revision: int64(b.RouteGeneration), User: b.AccountID, Team: teams[i], IssuedAt: d.IssuedAt, ExpiresAt: d.ExpiresAt}, s.Quota)
		if isExactQuotaExhaustion(err) {
			if s.QuotaDenied != nil {
				*s.QuotaDenied = append(*s.QuotaDenied, wire.IngressQuotaDenial{RouteID: b.RouteID, Hostname: b.Hostname})
			}
			continue
		}
		if err != nil {
			if err = s.rowFailed(ctx, "preview_accounting", b.RouteID, err); err != nil {
				return nil, err
			}
			continue
		}
		admitted = append(admitted, d)
	}
	return admitted, nil
}
