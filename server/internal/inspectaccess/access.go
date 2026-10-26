package inspectaccess

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// AccessRequest resolves an already issued grant to its current owner daemon.
// AccountID and CLIClientSessionID come only from authenticated server context.
type AccessRequest struct {
	AccountID          string `json:"-"`
	CLIClientSessionID string `json:"-"`
	Token              string `json:"credential_token"`
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	RouteID            string `json:"route_id"`
	Action             string `json:"action"`
	Transport          string `json:"transport"`
	EdgeNodeID         string `json:"edge_node_id,omitempty"`
	EdgeProcessEpoch   string `json:"process_epoch,omitempty"`
}
type CarrierRoute struct {
	LeaseGeneration      uint64 `json:"lease_generation"`
	AttachmentGeneration uint64 `json:"attachment_generation"`
	OwnerMachineID       string `json:"owner_machine_id"`
	MachineID            string `json:"machine_id"`
	RouteGeneration      uint64 `json:"route_generation"`
	AccountID            string `json:"account_id"`
	TunnelID             string `json:"tunnel_id"`
	ConnectorID          string `json:"connector_id"`
	SessionID            string `json:"session_id"`
	ProcessGeneration    uint64 `json:"process_generation"`
	Generation           uint64 `json:"generation"`
	RouteID              string `json:"route_id"`
	EdgeNodeID           string `json:"edge_node_id"`
	EdgeProcessEpoch     string `json:"edge_process_epoch"`
	AssignmentID         string `json:"assignment_id"`
	AssignmentGeneration uint64 `json:"assignment_generation"`
	ConfigContentHash    string `json:"config_content_hash"`
}
type AccessResult struct {
	CredentialID       string       `json:"credential_id"`
	OwnerAccountID     string       `json:"owner_account_id"`
	MachineID          string       `json:"machine_id"`
	MachineGeneration  uint64       `json:"machine_generation"`
	ResourceKind       string       `json:"resource_kind"`
	ResourceID         string       `json:"resource_id"`
	RouteID            string       `json:"route_id"`
	ResourceGeneration uint64       `json:"resource_generation"`
	RouteGeneration    uint64       `json:"route_generation"`
	TargetGeneration   uint64       `json:"target_generation"`
	ExpiresAt          time.Time    `json:"expires_at"`
	EdgeURL            string       `json:"edge_url"`
	Carrier            CarrierRoute `json:"carrier"`
}

func (s *Service) ResolveAccess(ctx context.Context, in AccessRequest) (AccessResult, error) {
	if ctx == nil || (in.Transport != "native" && in.Transport != "edge") || (in.Transport == "native" && !validID(in.CLIClientSessionID)) {
		return AccessResult{}, ErrInvalid
	}
	h := digest(in.Token)
	var owner, principal string
	var credentialExpiry time.Time
	err := s.db.SQL().QueryRowContext(ctx, `SELECT owner_account_id,account_id,expires_at FROM paperboat.inspector_credentials WHERE token_hash=$1 AND revoked_at IS NULL`, h[:]).Scan(&owner, &principal, &credentialExpiry)
	if isExactNoRows(err) {
		return AccessResult{}, ErrNotAuthorized
	}
	if err != nil {
		return AccessResult{}, err
	}
	if in.AccountID != "" && in.AccountID != principal {
		return AccessResult{}, ErrNotAuthorized
	}
	d, err := s.Authorize(ctx, AuthorizeRequest{MachineAccount: owner, Token: in.Token, ResourceKind: in.ResourceKind, ResourceID: in.ResourceID, RouteID: in.RouteID, Action: in.Action})
	if err != nil {
		return AccessResult{}, err
	}
	out := AccessResult{CredentialID: d.CredentialID, OwnerAccountID: owner, ResourceKind: d.ResourceKind, ResourceID: d.ResourceID, RouteID: d.RouteID, ResourceGeneration: d.ResourceGeneration, RouteGeneration: d.RouteGeneration, TargetGeneration: d.TargetGeneration, ExpiresAt: credentialExpiry}
	if in.Transport == "native" {
		return s.resolveNativeAccess(ctx, in, out)
	}
	c := &out.Carrier
	var endpoint string
	now := s.now()
	if in.ResourceKind == "preview" {
		err = s.db.SQL().QueryRowContext(ctx, `SELECT x.host_id,m.installation_generation,p.endpoint,x.account_id,x.tunnel_id,x.connector_id,x.connector_session_id,x.process_generation,x.config_generation,x.route_id,x.edge_node_id,x.edge_process_epoch,x.operation_id,x.attachment_generation,x.config_content_hash,x.lease_generation,x.attachment_generation,x.owner_machine_id
 FROM paperboat.preview_leases p JOIN paperboat.preview_routes x ON x.preview_id=p.id AND x.account_id=p.account_id JOIN paperboat.user_machines m ON m.id=x.host_id AND m.user_id=p.account_id JOIN paperboat.control_tunnel_nodes n ON n.id=x.edge_node_id AND n.process_epoch=x.edge_process_epoch
 WHERE p.id=$1 AND x.state='ready' AND x.expires_at>$2 AND x.route_generation=$3 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND n.state='ready' AND n.ready AND n.registry_expires_at>$2 ORDER BY x.operation_id LIMIT 1`, in.ResourceID, now, d.RouteGeneration).Scan(&out.MachineID, &out.MachineGeneration, &endpoint, &c.AccountID, &c.TunnelID, &c.ConnectorID, &c.SessionID, &c.ProcessGeneration, &c.Generation, &c.RouteID, &c.EdgeNodeID, &c.EdgeProcessEpoch, &c.AssignmentID, &c.AssignmentGeneration, &c.ConfigContentHash, &c.LeaseGeneration, &c.AttachmentGeneration, &c.OwnerMachineID)
	} else {
		// Tunnel routes ride the endpoint engine (PLAN 3a): the carrier is the agent's runtime carrier.
		err = s.db.SQL().QueryRowContext(ctx, `SELECT e.agent_machine_id,m.installation_generation,t.stable_endpoint,t.account_id,ra.admission->'binding'->>'tunnel_id',ra.admission->'binding'->>'connector_id',ra.session_id,ra.worker_generation,(ra.admission->'binding'->>'config_generation')::bigint,r.id,x.edge_node_id,x.edge_process_epoch,'epa_'||x.route_id,x.generation,ra.admission->>'config_content_hash'
 FROM paperboat.endpoint_edge_assignments x JOIN paperboat.endpoints e ON e.id=x.endpoint_id AND e.kind='tunnel' AND e.desired_state='active' JOIN paperboat.tunnels t ON t.id=e.tunnel_id JOIN paperboat.endpoint_routes er ON er.id=x.route_id JOIN paperboat.tunnel_routes r ON r.id=er.tunnel_route_id AND r.tunnel_id=t.id
 JOIN paperboat.runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id AND ra.edge_process_epoch=x.edge_process_epoch AND ra.session_id=x.carrier_session_id
 JOIN paperboat.user_machines m ON m.id=e.agent_machine_id AND m.user_id=t.account_id JOIN paperboat.control_tunnel_nodes n ON n.id=x.edge_node_id AND n.process_epoch=x.edge_process_epoch
 WHERE t.id=$1 AND r.id=$2 AND r.generation=$3 AND ($5='' OR (x.edge_node_id=$5 AND x.edge_process_epoch=$6)) AND x.desired='active' AND x.observed_ready AND ra.expires_at>$4 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND n.state='ready' AND n.ready AND n.registry_expires_at>$4 AND n.drain_deadline IS NULL
 ORDER BY x.edge_node_id LIMIT 1`, in.ResourceID, in.RouteID, d.RouteGeneration, now, in.EdgeNodeID, in.EdgeProcessEpoch).Scan(&out.MachineID, &out.MachineGeneration, &endpoint, &c.AccountID, &c.TunnelID, &c.ConnectorID, &c.SessionID, &c.ProcessGeneration, &c.Generation, &c.RouteID, &c.EdgeNodeID, &c.EdgeProcessEpoch, &c.AssignmentID, &c.AssignmentGeneration, &c.ConfigContentHash)
	}
	if isExactNoRows(err) {
		return AccessResult{}, ErrNotAuthorized
	}
	if err != nil {
		return AccessResult{}, err
	}
	c.MachineID = out.MachineID
	c.RouteGeneration = out.RouteGeneration
	if in.EdgeNodeID != "" && (c.EdgeNodeID != in.EdgeNodeID || c.EdgeProcessEpoch != in.EdgeProcessEpoch) {
		return AccessResult{}, ErrNotAuthorized
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(u.Host, "\r\n") {
		return AccessResult{}, ErrNotAuthorized
	}
	u.Path = "/.paperboat/inspector/v1"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	out.EdgeURL = u.String()

	return out, nil
}

func (s *Service) resolveNativeAccess(ctx context.Context, in AccessRequest, out AccessResult) (AccessResult, error) {
	now := s.now()
	var err error
	if in.ResourceKind == "preview" {
		err = s.db.SQL().QueryRowContext(ctx, `SELECT m.id,m.installation_generation FROM paperboat.preview_leases p JOIN paperboat.user_machines m ON m.id=p.owner_machine_id AND m.user_id=p.account_id WHERE p.id=$1 AND p.account_id=$2 AND p.terminal_state='active' AND p.lease_deadline>$3 AND m.deleted_at IS NULL AND m.revoked_at IS NULL`, in.ResourceID, out.OwnerAccountID, now).Scan(&out.MachineID, &out.MachineGeneration)
	} else {
		err = s.db.SQL().QueryRowContext(ctx, `SELECT m.id,m.installation_generation FROM paperboat.tunnels t JOIN paperboat.user_machines m ON m.id=t.created_by_host_id AND m.user_id=t.account_id WHERE t.id=$1 AND t.account_id=$2 AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$3) AND m.deleted_at IS NULL AND m.revoked_at IS NULL`, in.ResourceID, out.OwnerAccountID, now).Scan(&out.MachineID, &out.MachineGeneration)
	}
	if isExactNoRows(err) {
		return AccessResult{}, ErrNotAuthorized
	}
	if err != nil {
		return AccessResult{}, err
	}

	result, e := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.inspector_credentials i SET native_cli_session_id=$1,native_machine_id=$2,native_machine_generation=$3 WHERE i.credential_id=$4 AND i.revoked_at IS NULL AND i.expires_at>$5 AND (i.native_cli_session_id IS NULL OR i.native_cli_session_id=$1) AND (i.native_machine_id IS NULL OR (i.native_machine_id=$2 AND i.native_machine_generation=$3)) AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c WHERE c.id=$1 AND c.user_id=i.account_id AND c.state='active' AND c.revoked_at IS NULL)`, in.CLIClientSessionID, out.MachineID, out.MachineGeneration, out.CredentialID, now)
	if e != nil {
		return AccessResult{}, e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return AccessResult{}, e
	}
	if n != 1 {
		return AccessResult{}, ErrNotAuthorized
	}
	return out, nil
}
