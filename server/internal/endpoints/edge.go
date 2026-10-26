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
	admissions, _, err := EdgeAdmissionsPage(ctx, q, nodeID, epoch, Position{}, expiresAt, limit)
	return admissions, err
}

// Position is a keyset position in an edge's endpoint assignments, ordered by carrier session and
// route id; the zero Position is before the first.
type Position struct{ Session, Route string }

// EdgeAdmissionsPage is EdgeAdmissions from after the given position, at most limit+1 rows (the
// extra row tells the caller there is more), with the position of the last row returned.
func EdgeAdmissionsPage(ctx context.Context, q Querier, nodeID, epoch string, after Position, expiresAt time.Time, limit int) ([]runtimewire.Admission, Position, error) {
	rows, err := q.Query(ctx, `SELECT x.carrier_session_id,x.route_id,a.admission,x.generation,r.hostname,r.revision,e.id,e.access_mode,true
FROM endpoint_edge_assignments x JOIN endpoint_routes r ON r.id=x.route_id JOIN endpoints e ON e.id=x.endpoint_id
JOIN runtime_carrier_attachments a ON a.machine_id=e.agent_machine_id AND a.edge_node_id=x.edge_node_id
 AND a.edge_process_epoch=x.edge_process_epoch AND a.session_id=x.carrier_session_id
 AND a.agent_capabilities @> ARRAY['endpoints']::text[]
WHERE x.edge_node_id=$1 AND x.edge_process_epoch=$2 AND x.desired='active' AND e.kind='preview' AND `+liveEndpoint+`
AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,false)
AND (x.carrier_session_id,x.route_id)>($4,$5)
ORDER BY x.carrier_session_id,x.route_id LIMIT $3`, nodeID, epoch, limit+1, after.Session, after.Route)
	if err != nil {
		return nil, Position{}, err
	}
	return scanEdgeAdmissions(rows, expiresAt)
}

// EdgeAdmissionsChanged returns the endpoint assignments of one edge process whose admission may
// have changed after since: each is an admission (as EdgeAdmissions would answer it) or a removed
// route id. Callers ask from a little before their last answer, so a change committed late is not
// missed; answering a row twice is harmless.
func EdgeAdmissionsChanged(ctx context.Context, q Querier, nodeID, epoch string, since time.Time, after Position, expiresAt time.Time, limit int) ([]runtimewire.Admission, []string, Position, error) {
	rows, err := q.Query(ctx, `SELECT x.carrier_session_id,x.route_id,a.admission,x.generation,r.hostname,r.revision,e.id,e.access_mode,
 coalesce(x.desired='active' AND a.route_id IS NOT NULL AND `+liveEndpoint+`
 AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,false),false)
FROM endpoint_edge_assignments x JOIN endpoint_routes r ON r.id=x.route_id JOIN endpoints e ON e.id=x.endpoint_id
LEFT JOIN runtime_carrier_attachments a ON a.machine_id=e.agent_machine_id AND a.edge_node_id=x.edge_node_id
 AND a.edge_process_epoch=x.edge_process_epoch AND a.session_id=x.carrier_session_id
 AND a.agent_capabilities @> ARRAY['endpoints']::text[]
WHERE x.edge_node_id=$1 AND x.edge_process_epoch=$2 AND x.changed_at>$3 AND e.kind='preview'
AND (x.carrier_session_id,x.route_id)>($5,$6)
ORDER BY x.carrier_session_id,x.route_id LIMIT $4`, nodeID, epoch, since, limit+1, after.Session, after.Route)
	if err != nil {
		return nil, nil, Position{}, err
	}
	return scanEdgeAdmissionsWithRemovals(rows, expiresAt)
}

// scanEdgeAdmissions builds each row into its endpoint admission and returns the position of the
// last row.
func scanEdgeAdmissions(rows pgx.Rows, expiresAt time.Time) ([]runtimewire.Admission, Position, error) {
	admissions, _, last, err := scanEdgeAdmissionsWithRemovals(rows, expiresAt)
	return admissions, last, err
}

// scanEdgeAdmissionsWithRemovals builds each live row into its endpoint admission and returns the
// other rows' route ids as removed, with the position of the last row.
func scanEdgeAdmissionsWithRemovals(rows pgx.Rows, expiresAt time.Time) ([]runtimewire.Admission, []string, Position, error) {
	defer rows.Close()
	out, removed := []runtimewire.Admission{}, []string{}
	var last Position
	for rows.Next() {
		var raw []byte
		var session, route, hostname, endpoint, access string
		var generation, revision int64
		var live bool
		if err := rows.Scan(&session, &route, &raw, &generation, &hostname, &revision, &endpoint, &access, &live); err != nil {
			return nil, nil, Position{}, err
		}
		last = Position{Session: session, Route: route}
		if !live {
			removed = append(removed, route)
			continue
		}
		var a runtimewire.Admission
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, nil, Position{}, err
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
	return out, removed, last, rows.Err()
}

// ObserveTx records an edge's observation of one endpoint route. It reports false when the
// observation is not for a current endpoint assignment (for example one removed since the edge's
// last pull); such observations are dropped, never an error, so they cannot fail the observations
// of other routes in the same report.
//
// Edges repeat every observation every few seconds; an unchanged one is written only to refresh observed_at
// once it is older than ObservationRefresh, otherwise it is only read.
func ObserveTx(ctx context.Context, tx *db.Tx, nodeID, epoch string, o runtimewire.Observation) (bool, error) {
	var current bool
	err := tx.QueryRow(ctx, `WITH assignment AS (
SELECT route_id,edge_node_id,(observed_ready IS DISTINCT FROM ($6 AND desired='active') OR observed_at IS NULL OR observed_at<now()-$7::interval) AS due
FROM endpoint_edge_assignments
WHERE route_id=$1 AND edge_node_id=$2 AND edge_process_epoch=$3 AND carrier_session_id=$4 AND generation=$5
), written AS (
UPDATE endpoint_edge_assignments x SET observed_ready=$6 AND x.desired='active',observed_at=now()
FROM assignment a WHERE a.due AND x.route_id=a.route_id AND x.edge_node_id=a.edge_node_id
RETURNING x.route_id
)
SELECT EXISTS(SELECT 1 FROM assignment)`,
		o.RouteID, nodeID, epoch, o.SessionID, int64(o.AttachmentGeneration), o.Ready, ObservationRefresh.String()).Scan(&current)
	return current, err
}

// ObservationRefresh is how stale an unchanged edge observation may get before it is written again; readers
// treat an observation as current for at least 15 s (tunnelv1 health projection).
const ObservationRefresh = 10 * time.Second

// IsRouteTx reports whether a route id names an endpoint route (as opposed to a runtime route).
func IsRouteTx(ctx context.Context, tx *db.Tx, routeID string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoint_routes WHERE id=$1)`, routeID).Scan(&found)
	return found, err
}
