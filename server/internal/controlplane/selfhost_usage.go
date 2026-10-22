package controlplane

import "context"

// Independent operator signatures prove origin, not ownership of a named
// account's traffic. Bind every report to this node's recorded assignment and
// a still-authorized pairing before accepting its counters.
func (s *EdgeService) authorizeSelfhostUsage(ctx context.Context, r edgeUsageRequest) (bool, error) {
	if s.nodeCredentials == nil {
		return false, nil
	}
	private, err := s.nodeCredentials.IsSelfHosted(ctx, r.Node)
	if err != nil || !private {
		return private, err
	}
	var authorized bool
	err = s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS (
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
 )`, r.Node, r.Route, r.Revision, r.Environment).Scan(&authorized)
	if err != nil {
		return true, err
	}
	if !authorized {
		return true, ErrUsageSignature
	}
	// Installation credentials also cover globally operated nodes. Only
	// account/team ownership exempts accepted telemetry from hosted billing.
	var customerOwned bool
	err = s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=$1 AND scope_kind IN ('account','team'))`, r.Node).Scan(&customerOwned)
	return customerOwned, err
}
