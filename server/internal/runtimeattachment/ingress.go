package runtimeattachment

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"net/url"
	"strings"
)

// IngressURL preserves the runtime's authenticated logical hostname while
// customer traffic uses the enrolled edge's infrastructure TLS hostname.
func IngressURL(ctx context.Context, database *db.DB, environmentID, publicHost string) (string, error) {
	found, err := IngressURLs(ctx, database, map[string]string{environmentID: publicHost})
	if err != nil {
		return "", err
	}
	return found[environmentID].URL, found[environmentID].Err
}

// Ingress is one environment's runtime ingress URL, or why it has none (pgx.ErrNoRows: no such runtime route;
// ErrUnavailable: not reachable through an edge now).
type Ingress struct {
	URL string
	Err error
}

// IngressURLs is IngressURL for several environments (environment -> runtime public host) in two queries,
// whatever their number; every environment asked about has an entry.
func IngressURLs(ctx context.Context, database *db.DB, hosts map[string]string) (map[string]Ingress, error) {
	out := make(map[string]Ingress, len(hosts))
	environments, publicHosts := make([]string, 0, len(hosts)), make([]string, 0, len(hosts))
	for environment, host := range hosts {
		out[environment] = Ingress{Err: pgx.ErrNoRows}
		environments, publicHosts = append(environments, environment), append(publicHosts, host)
	}
	rows, err := database.Pool().Query(ctx, `SELECT m.environment_id,paperboat.workspace_edge_mode(m.user_id,m.owner_team_id),r.managed_public_host
 FROM unnest($1::text[],$2::text[]) AS w(environment_id,public_host)
 JOIN paperboat.user_machines m ON m.environment_id=w.environment_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 JOIN paperboat.control_routes r ON r.environment_id=m.environment_id AND r.connector_id='runtime' AND r.kind='runtime_https_wss' AND r.public_host=w.public_host`,
		environments, publicHosts)
	if err != nil {
		return nil, err
	}
	var selfHosted, selfHostedHosts []string
	for rows.Next() {
		var environment, mode, managedHost string
		if err := rows.Scan(&environment, &mode, &managedHost); err != nil {
			rows.Close()
			return nil, err
		}
		publicHost := hosts[environment]
		switch {
		case mode == "managed-only" && publicHost != "" && publicHost == managedHost:
			out[environment] = Ingress{URL: (&url.URL{Scheme: "https", Host: publicHost}).String()}
		case mode == "self-hosted-only" && publicHost != managedHost:
			out[environment] = Ingress{Err: ErrUnavailable} // until an edge carrier is found below
			selfHosted, selfHostedHosts = append(selfHosted, environment), append(selfHostedHosts, publicHost)
		default:
			out[environment] = Ingress{Err: ErrUnavailable}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(selfHosted) == 0 {
		return out, nil
	}
	rows, err = database.Pool().Query(ctx, `SELECT DISTINCT ON (r.environment_id) r.environment_id,n.endpoint_host
 FROM unnest($1::text[],$2::text[]) AS w(environment_id,public_host)
 JOIN paperboat.control_routes r ON r.environment_id=w.environment_id AND r.public_host=w.public_host
 JOIN paperboat.runtime_carrier_attachments a ON a.route_id=r.id
 JOIN paperboat.control_tunnel_nodes n ON n.id=a.edge_node_id
 WHERE a.active AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,true)
 ORDER BY r.environment_id,a.edge_node_id`, selfHosted, selfHostedHosts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var environment, edgeHost string
		if err := rows.Scan(&environment, &edgeHost); err != nil {
			return nil, err
		}
		publicHost := hosts[environment]
		if edgeHost == "" || publicHost == "" || strings.ContainsAny(edgeHost+publicHost, "/:@?# \t\r\n") {
			continue
		}
		out[environment] = Ingress{URL: (&url.URL{Scheme: "https", Host: edgeHost, Path: "/v1/runtime/" + publicHost}).String()}
	}
	return out, rows.Err()
}
