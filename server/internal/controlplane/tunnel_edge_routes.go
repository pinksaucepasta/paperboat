package controlplane

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func (s *EdgeService) ListTunnelEdgeRouteAssignmentsForNodeV1(ctx context.Context, nodeID, processEpoch string) ([]dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Row, error) {
	if !validTunnelEdgeIdentifier(nodeID) || !validTunnelEdgeProcessEpoch(processEpoch) || s == nil || s.store == nil {
		return nil, ErrAssignmentInvalid
	}
	rows, err := s.store.Queries().ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Params{
		EdgeNodeID: nodeID, EdgeProcessEpoch: processEpoch, Now: sql.NullTime{Time: s.clock().UTC(), Valid: true},
	})
	return rows, err
}

func validTunnelEdgeIdentifier(value string) bool {
	if value != strings.TrimSpace(value) || len(value) < 3 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '.' || character == ':' || character == '-' {
			if index == 0 && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func validTunnelEdgeProcessEpoch(value string) bool {
	if value != strings.TrimSpace(value) || len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// CanonicalRouteSnapshot is the process-fenced tunnel route snapshot an edge process pulls. An
// engine route (PLAN 3a step 4) rides on the agent's runtime carrier: its machine thumbprint is the
// shared connector thumbprint of the agent's key, the one the edge's durable admission registry
// checks, computed here by the same shared function (connectorprotocol.IdentityThumbprint).
func (s *EdgeService) CanonicalRouteSnapshot(ctx context.Context, nodeID, processEpoch string) ([]map[string]any, error) {
	rows, err := s.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, nodeID, processEpoch)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row.MachineIdentityThumbprint == "" {
			key, err := base64.RawURLEncoding.Strict().DecodeString(row.MachineIdentityPublicKey)
			if err != nil {
				return nil, fmt.Errorf("engine route %s machine key: %w", row.RouteID, err)
			}
			if row.MachineIdentityThumbprint, err = connectorprotocol.IdentityThumbprint(ed25519.PublicKey(key)); err != nil {
				return nil, fmt.Errorf("engine route %s machine thumbprint: %w", row.RouteID, err)
			}
		}
		items = append(items, tunnelEdgeRouteJSON(row))
	}
	return items, nil
}

func tunnelEdgeRouteJSON(row dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Row) map[string]any {
	// The target is an opaque connector route binding. The edge must open the
	// canonical stream on the authenticated connector; it must never dial the
	// host-local origin address from this projection.
	value := map[string]any{
		"assignment_id":                row.AssignmentID,
		"assignment_generation":        row.AssignmentGeneration,
		"route_id":                     row.RouteID,
		"route_revision":               row.RouteRevision,
		"account_id":                   row.AccountID,
		"host_id":                      row.HostID,
		"machine_identity_public_key":  row.MachineIdentityPublicKey,
		"machine_identity_thumbprint":  row.MachineIdentityThumbprint,
		"tunnel_id":                    row.TunnelID,
		"connector_id":                 row.ConnectorID,
		"connector_generation":         row.ConnectorGeneration,
		"connector_session_id":         row.ConnectorSessionID,
		"connector_process_generation": row.ConnectorProcessGeneration,
		"config_generation":            row.ConfigGeneration,
		"config_content_hash":          "sha256:" + hex.EncodeToString(row.ConfigContentHash),
		"access_mode":                  row.AccessMode,
		"viewer_policy_generation":     row.ViewerPolicyGeneration,
		"edge_node_id":                 row.EdgeNodeID,
		"edge_process_epoch":           row.EdgeProcessEpoch,
		"edge_failure_domain":          row.EdgeFailureDomain,
		"kind":                         row.Kind,
		"public_host":                  row.PublicHost,
		"match_type":                   row.MatchType,
		"match_hostname":               nullableTunnelEdgeText(row.MatchHostname),
		"wildcard_suffix":              nullableTunnelEdgeText(row.WildcardSuffix),
		"path_prefix":                  nullableTunnelEdgeText(row.PathPrefix),
		"priority":                     row.Priority,
		"protocol":                     row.Protocol,
		"public_tcp_listener_id":       nullableTunnelEdgeText(row.PublicTcpListenerID),
		"public_tcp_port":              nullableTunnelEdgeInt32(row.PublicTcpPort),
		"origin_scheme":                row.OriginScheme,
		"preserve_host":                row.PreserveHost,
		"host_override":                nullableTunnelEdgeText(row.HostOverride),
		"domain_bindings":              json.RawMessage([]byte(row.DomainBindings)),
		"state":                        row.State,
		"desired_state":                row.RouteDesiredState,
		"observed_state":               row.ObservedState,
		"target": map[string]any{
			"route_id":                     row.RouteID,
			"connector_session_id":         row.ConnectorSessionID,
			"connector_process_generation": row.ConnectorProcessGeneration,
			"config_generation":            row.ConfigGeneration,
		},
	}
	if row.Protocol == "private_tcp" {
		// A private TCP route is an access-only carrier binding with no host, path or priority of its
		// own: the tunnel host, stored catch-all match and default priority are not part of what the edge is
		// told (the edge refuses a private TCP assignment that carries them).
		value["public_host"], value["match_type"], value["priority"] = "", "", 0
	}
	if row.ScopedDomainHostname != "" {
		value["public_host"] = row.ScopedDomainHostname
		value["match_type"] = row.ScopedDomainMatchType
		value["match_hostname"] = row.ScopedDomainHostname
		value["wildcard_suffix"] = nil
		if row.ScopedDomainMatchType == "one_label_wildcard" {
			value["match_hostname"] = nil
			value["wildcard_suffix"] = strings.TrimPrefix(row.ScopedDomainHostname, "*.")
		}
	}
	return value
}

func nullableTunnelEdgeText(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableTunnelEdgeInt32(value sql.NullInt32) any {
	if !value.Valid {
		return nil
	}
	return value.Int32
}
