package controlplane

import (
 wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
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
	quota, _ := s.scopedBandwidth.(accountingauthority.QuotaChecker)
	decisions, err := (connectorprotocol.SQLIngressSource{DB: s.store, Quota: quota}).Edge(r.Context(), input.NodeID, input.ProcessEpoch)
	if err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeEdgeJSON(w, http.StatusOK, map[string]any{"complete": true, "decisions": decisions})
}
