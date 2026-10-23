package endpoints

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Execer is a pool or a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// RecordEdgeServes notes that this edge process admits endpoint routes (it pulled its runtime
// snapshot advertising the capability) and, with tunnels, tunnel routes on agent carriers.
func RecordEdgeServes(ctx context.Context, db Execer, nodeID, epoch string, tunnels bool) error {
	_, err := db.Exec(ctx, `UPDATE control_tunnel_nodes SET endpoints_epoch=$2,
tunnel_endpoints_epoch=CASE WHEN $3 THEN $2 ELSE tunnel_endpoints_epoch END
WHERE id=$1 AND process_epoch=$2 AND (endpoints_epoch IS DISTINCT FROM $2 OR $3 AND tunnel_endpoints_epoch IS DISTINCT FROM $2)`, nodeID, epoch, tunnels)
	return err
}

// EdgesServeTx reports whether every given edge process admits endpoint routes, and with tunnels,
// tunnel routes on agent carriers.
func EdgesServeTx(ctx context.Context, tx *db.Tx, nodes, epochs []string, tunnels bool) (bool, error) {
	if len(nodes) == 0 || len(nodes) != len(epochs) {
		return false, nil
	}
	var served int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM control_tunnel_nodes n JOIN unnest($1::text[],$2::text[]) AS e(node,epoch) ON n.id=e.node
AND n.endpoints_epoch=e.epoch AND (NOT $3 OR n.tunnel_endpoints_epoch=e.epoch)`, nodes, epochs, tunnels).Scan(&served)
	return served == len(nodes), err
}

// EdgeAdmissions returns the endpoint route admissions for one edge process: each live assignment
// whose agent runtime carrier on this edge is still valid becomes an admission bound to that
// carrier's identity, with the route's own id, hostname and kind. Callers send these only to an
// edge that advertised runtimewire.CapabilityEndpoints.
func EdgeAdmissions(ctx context.Context, q Querier, nodeID, epoch string, expiresAt time.Time, limit int) ([]runtimewire.Admission, error) {
	rows, err := q.Query(ctx, `SELECT a.admission,x.route_id,x.generation,r.hostname,r.revision,e.id,e.access_mode
FROM endpoint_edge_assignments x JOIN endpoint_routes r ON r.id=x.route_id JOIN endpoints e ON e.id=x.endpoint_id
JOIN runtime_carrier_attachments a ON a.machine_id=e.agent_machine_id AND a.edge_node_id=x.edge_node_id
 AND a.edge_process_epoch=x.edge_process_epoch AND a.session_id=x.carrier_session_id
 AND a.agent_capabilities @> ARRAY['endpoints']::text[]
WHERE x.edge_node_id=$1 AND x.edge_process_epoch=$2 AND x.desired='active' AND e.kind='preview' AND `+liveEndpoint+`
AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,false)
ORDER BY x.route_id LIMIT $3`, nodeID, epoch, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []runtimewire.Admission{}
	for rows.Next() {
		var raw []byte
		var route, hostname, endpoint, access string
		var generation, revision int64
		if err := rows.Scan(&raw, &route, &generation, &hostname, &revision, &endpoint, &access); err != nil {
			return nil, err
		}
		var a runtimewire.Admission
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		a.Binding.RouteID, a.Binding.RouteGeneration = route, uint64(revision)
		a.RouteRevision, a.AttachmentGeneration = uint64(revision), uint64(generation)
		a.Hostname, a.EndpointID, a.AccessMode = hostname, endpoint, access
		a.RouteKind = runtimewire.EndpointPublicRouteKind
		if access != "public" {
			a.RouteKind = runtimewire.EndpointRestrictedRouteKind
		}
		a.ExpiresAt = expiresAt
		out = append(out, a)
	}
	return out, rows.Err()
}

// ObserveTx records an edge's observation of one endpoint route. It reports false when the
// observation is not for a current endpoint assignment (for example one removed since the edge's
// last pull); such observations are dropped, never an error, so they cannot fail the observations
// of other routes in the same report.
func ObserveTx(ctx context.Context, tx *db.Tx, nodeID, epoch string, o runtimewire.Observation) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE endpoint_edge_assignments SET observed_ready=$6 AND desired='active',observed_at=now()
WHERE route_id=$1 AND edge_node_id=$2 AND edge_process_epoch=$3 AND carrier_session_id=$4 AND generation=$5`,
		o.RouteID, nodeID, epoch, o.SessionID, int64(o.AttachmentGeneration), o.Ready)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// IsRouteTx reports whether a route id names an endpoint route (as opposed to a runtime route).
func IsRouteTx(ctx context.Context, tx *db.Tx, routeID string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoint_routes WHERE id=$1)`, routeID).Scan(&found)
	return found, err
}
