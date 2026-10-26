package db

import "context"

// EdgeHoldsUsageRoute reports whether an edge node serves the route an independent operator's
// usage report names, at the revision and environment of the ingress decision the edge metered
// it under. A route is held through a connector assignment, a preview route, a runtime carrier
// attachment, or (PLAN 3a step 4) an engine tunnel's endpoint_edge_assignments, whose ingress
// decision carries the tunnel route generation and the agent machine's environment. One owner
// for the question, used by the usage endpoint and tested against each path's fixtures.
func (d *DB) EdgeHoldsUsageRoute(ctx context.Context, node, route string, revision int64, environment string) (bool, error) {
	var held bool
	err := d.SQL().QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM paperboat.tunnel_edge_route_assignments a
 JOIN paperboat.user_machines m ON m.id=a.host_id AND m.user_id=a.account_id
 WHERE a.edge_node_id=$1 AND a.route_id=$2 AND a.route_generation=$3 AND m.environment_id=$4
 AND paperboat.node_route_eligible(a.edge_node_id,a.account_id,a.route_id)
 UNION ALL
 SELECT 1 FROM paperboat.preview_routes a
 JOIN paperboat.user_machines m ON m.id=a.owner_machine_id
 WHERE a.edge_node_id=$1 AND a.route_id=$2 AND a.route_generation=$3 AND m.environment_id=$4
 AND paperboat.node_preview_eligible(a.edge_node_id,a.account_id,a.preview_id)
 UNION ALL
 SELECT 1 FROM paperboat.runtime_carrier_attachments attachment
 JOIN paperboat.control_routes r ON r.id=attachment.route_id
 JOIN paperboat.control_environments e ON e.id=r.environment_id
 WHERE attachment.edge_node_id=$1 AND r.id=$2 AND attachment.route_revision=$3 AND e.id=$4
 AND paperboat.runtime_carrier_valid(r.id,attachment.edge_node_id,attachment.edge_process_epoch,false)
 UNION ALL
 SELECT 1 FROM paperboat.endpoint_edge_assignments x
 JOIN paperboat.endpoints e ON e.id=x.endpoint_id AND e.kind='tunnel' AND e.desired_state='active'
 JOIN paperboat.tunnels t ON t.id=e.tunnel_id
 JOIN paperboat.endpoint_routes er ON er.id=x.route_id
 JOIN paperboat.tunnel_routes r ON r.id=er.tunnel_route_id AND r.tunnel_id=t.id
 JOIN paperboat.user_machines m ON m.id=e.agent_machine_id AND m.user_id=t.account_id
 JOIN paperboat.control_tunnel_nodes node ON node.id=x.edge_node_id AND node.process_epoch=x.edge_process_epoch
 WHERE x.edge_node_id=$1 AND r.id=$2 AND r.generation=$3 AND m.environment_id=$4 AND x.desired='active'
 )`, node, route, revision, environment).Scan(&held)
	return held, err
}
