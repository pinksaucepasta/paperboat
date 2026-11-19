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
	// Installation credentials also cover Paperboat's own (global) nodes. Their reports are
	// hosted traffic, billed through the scoped authority path like any managed edge; binding
	// them to the node's current assignment refused every report for a route that had since
	// moved to another edge, and the edge retried it forever.
	if customerOwned, err := s.customerOwnedNode(ctx, r.Node); err != nil || !customerOwned {
		return false, err
	}
	authorized, err := s.store.EdgeHoldsUsageRoute(ctx, r.Node, r.Route, r.Revision, r.Environment)
	if err != nil {
		return true, err
	}
	if !authorized {
		return true, usageSignatureError("an independent operator's report names a route this node does not hold")
	}
	return true, nil
}
