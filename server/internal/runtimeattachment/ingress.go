package runtimeattachment

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"net/url"
)

// IngressURL is the runtime route's address: Paperboat's own hostname for the machine, whatever the workspace's edge
// mode. Browser terminals and the rest of the runtime route (file inbox, preview dispatch) never use a customer-run
// edge, so a self-hosted-only workspace is reached the same way through its machine's managed carrier.
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

// IngressURLs is IngressURL for several environments (environment -> runtime public host) in one query,
// whatever their number; every environment asked about has an entry.
func IngressURLs(ctx context.Context, database *db.DB, hosts map[string]string) (map[string]Ingress, error) {
	out := make(map[string]Ingress, len(hosts))
	environments, publicHosts := make([]string, 0, len(hosts)), make([]string, 0, len(hosts))
	for environment, host := range hosts {
		out[environment] = Ingress{Err: pgx.ErrNoRows}
		environments, publicHosts = append(environments, environment), append(publicHosts, host)
	}
	rows, err := database.Pool().Query(ctx, `SELECT m.environment_id,r.managed_public_host
 FROM unnest($1::text[],$2::text[]) AS w(environment_id,public_host)
 JOIN paperboat.user_machines m ON m.environment_id=w.environment_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 JOIN paperboat.control_routes r ON r.environment_id=m.environment_id AND r.connector_id='runtime' AND r.kind='runtime_https_wss' AND r.public_host=w.public_host`,
		environments, publicHosts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var environment, managedHost string
		if err := rows.Scan(&environment, &managedHost); err != nil {
			return nil, err
		}
		publicHost := hosts[environment]
		out[environment] = Ingress{Err: ErrUnavailable}
		if publicHost != "" && publicHost == managedHost {
			out[environment] = Ingress{URL: (&url.URL{Scheme: "https", Host: publicHost}).String()}
		}
	}
	return out, rows.Err()
}
