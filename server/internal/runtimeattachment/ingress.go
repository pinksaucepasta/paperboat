package runtimeattachment

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"net/url"
	"strings"
)

// IngressURL preserves the runtime's authenticated logical hostname while
// customer traffic uses the enrolled edge's infrastructure TLS hostname.
func IngressURL(ctx context.Context, database *db.DB, environmentID, publicHost string) (string, error) {
	var mode, managedHost string
	err := database.Pool().QueryRow(ctx, `SELECT paperboat.workspace_edge_mode(m.user_id,m.owner_team_id),r.managed_public_host FROM paperboat.user_machines m JOIN paperboat.control_routes r ON r.environment_id=m.environment_id AND r.connector_id='runtime' AND r.kind='runtime_https_wss' WHERE m.environment_id=$1 AND r.public_host=$2 AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, environmentID, publicHost).Scan(&mode, &managedHost)
	if err != nil {
		return "", err
	}
	if mode == "managed-only" {
		if publicHost == "" || publicHost != managedHost {
			return "", ErrUnavailable
		}
		return (&url.URL{Scheme: "https", Host: publicHost}).String(), nil
	}
	if mode != "self-hosted-only" || publicHost == managedHost {
		return "", ErrUnavailable
	}
	var edgeHost string
	err = database.Pool().QueryRow(ctx, `SELECT n.endpoint_host FROM paperboat.runtime_carrier_attachments a JOIN paperboat.control_routes r ON r.id=a.route_id JOIN paperboat.control_tunnel_nodes n ON n.id=a.edge_node_id WHERE r.environment_id=$1 AND r.public_host=$2 AND a.active AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,true) ORDER BY a.edge_node_id LIMIT 1`, environmentID, publicHost).Scan(&edgeHost)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnavailable
	}
	if err != nil {
		return "", err
	}
	if edgeHost == "" || publicHost == "" || strings.ContainsAny(edgeHost+publicHost, "/:@?# \t\r\n") {
		return "", ErrUnavailable
	}
	return (&url.URL{Scheme: "https", Host: edgeHost, Path: "/v1/runtime/" + publicHost}).String(), nil
}
