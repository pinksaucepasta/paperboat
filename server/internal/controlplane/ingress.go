package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
)

// handleIngress is mounted only beneath the authenticated edge-control mux.
// A complete empty snapshot revokes authority; failed reads never renew it.
func (s *EdgeService) handleIngress(w http.ResponseWriter, r *http.Request) {
	var input struct {
		NodeID       string `json:"edge_node_id"`
		ProcessEpoch string `json:"process_epoch"`
	}
	if !s.decode(w, r, &input) {
		return
	}
	if wire.ValidateIdentifier(input.NodeID) != nil || wire.ValidateOpaqueEpoch(input.ProcessEpoch) != nil {
		writeEdgeError(w, r, http.StatusBadRequest, "invalid_request", false, 0)
		return
	}
	// Only a verified public edge is served ingress decisions. A relay, or an edge whose public ingress has not
	// been verified, gets a clear final answer instead of a retryable 5xx it would keep polling.
	if state, err := s.ingressNodeState(r.Context(), input.NodeID); err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	} else if state != ingressNodeVerifiedEdge {
		writeEdgeError(w, r, http.StatusConflict, state, false, 0)
		return
	}
	quota, _ := s.scopedBandwidth.(accountingauthority.QuotaChecker)
	var denied []wire.IngressQuotaDenial
	decisions, err := (connectorprotocol.SQLIngressSource{DB: s.store, Quota: quota, QuotaDenied: &denied}).Edge(r.Context(), input.NodeID, input.ProcessEpoch)
	if err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(denied) > 4096 {
		denied = denied[:4096]
	}
	writeEdgeJSON(w, http.StatusOK, map[string]any{"complete": true, "decisions": decisions, "quota_denied": denied})
}

const (
	ingressNodeVerifiedEdge = "verified_edge"
	ingressNodeNotEdge      = "node_not_edge"
	ingressNodeNotVerified  = "node_not_verified"
)

// ingressNodeState says whether the node may be served ingress decisions: a node with the edge role whose public
// ingress address has been verified. Unknown nodes are reported as not an edge.
func (s *EdgeService) ingressNodeState(ctx context.Context, nodeID string) (string, error) {
	var isEdge, verified bool
	err := s.store.SQL().QueryRowContext(ctx, `SELECT roles @> ARRAY['edge']::text[], public_ingress_verified_at IS NOT NULL FROM control_tunnel_nodes WHERE id=$1`, nodeID).Scan(&isEdge, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return ingressNodeNotEdge, nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case !isEdge:
		return ingressNodeNotEdge, nil
	case !verified:
		return ingressNodeNotVerified, nil
	}
	return ingressNodeVerifiedEdge, nil
}
