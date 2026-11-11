package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"net/http"
	"slices"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"

	"github.com/pinksaucepasta/paperboat/server/internal/ingressprojection"
)

// HTTP01Source supplies the http-01 responses an edge must serve. ForEdge also
// records that the edge has fetched them, which the issuer waits for.
type HTTP01Source interface {
	ForEdge(nodeID string) []wire.HTTP01Response
}

// handleIngress is mounted only beneath the authenticated edge-control mux.
// A complete empty snapshot revokes authority; failed reads never renew it.
func (s *EdgeService) handleIngress(w http.ResponseWriter, r *http.Request) {
	var input struct {
		NodeID       string `json:"edge_node_id"`
		ProcessEpoch string `json:"process_epoch"`
		// Version is the version of the answer the edge holds; when it is still current only fresh
		// times come back (F21-38).
		Version string `json:"version,omitempty"`
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
	decisions, err := (ingressprojection.SQLIngressSource{DB: s.store, Quota: quota, QuotaDenied: &denied}).Edge(r.Context(), input.NodeID, input.ProcessEpoch)
	if err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	}
	blocked, err := withholdBlocked(r.Context(), s.store, &decisions, &denied)
	if err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(denied) > 4096 {
		denied = denied[:4096]
	}
	var responses []wire.HTTP01Response
	// The field exists only where http-01 is configured (staging).
	if s.http01 != nil {
		responses = s.http01.ForEdge(input.NodeID)
	}
	version := wire.IngressVersion(decisions, denied, blocked, responses)
	if input.Version == version {
		expires := make([]time.Time, len(decisions))
		for i, d := range decisions {
			expires[i] = d.ExpiresAt
		}
		writeEdgeJSON(w, http.StatusOK, map[string]any{"complete": true, "unchanged": true, "version": version, "issued_at": latestIssuedAt(decisions), "expires_at": expires})
		return
	}
	snapshotBody := map[string]any{"complete": true, "version": version, "decisions": decisions, "quota_denied": denied, "blocked": blocked}
	if s.http01 != nil {
		snapshotBody["http01_responses"] = responses
	}
	writeEdgeJSON(w, http.StatusOK, snapshotBody)
}

// latestIssuedAt is the newest issue time of the answer; a kept decision never gets a longer window.
func latestIssuedAt(decisions []wire.IngressDecision) (latest time.Time) {
	for _, d := range decisions {
		if d.IssuedAt.After(latest) {
			latest = d.IssuedAt
		}
	}
	return latest
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

// withholdBlocked removes the decisions and quota denials of hostnames an operator took down
// (publication_blocks) and returns those hostnames, so the edge answers the takedown page without
// contacting the origin. A name is listed only when this edge has a decision or denial for it.
func withholdBlocked(ctx context.Context, store *db.DB, decisions *[]wire.IngressDecision, denied *[]wire.IngressQuotaDenial) ([]wire.IngressBlock, error) {
	names := make([]string, 0, len(*decisions)+len(*denied))
	for _, d := range *decisions {
		names = append(names, d.Binding.Hostname)
	}
	for _, d := range *denied {
		names = append(names, d.Hostname)
	}
	blocked := []wire.IngressBlock{}
	if len(names) == 0 {
		return blocked, nil
	}
	rows, err := store.Pool().Query(ctx, `SELECT hostname, legal FROM publication_blocks WHERE blocked AND hostname = ANY($1::text[])`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var b wire.IngressBlock
		if err := rows.Scan(&b.Hostname, &b.Legal); err != nil {
			return nil, err
		}
		taken[b.Hostname] = true
		blocked = append(blocked, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	*decisions = slices.DeleteFunc(*decisions, func(d wire.IngressDecision) bool { return taken[d.Binding.Hostname] })
	*denied = slices.DeleteFunc(*denied, func(d wire.IngressQuotaDenial) bool { return taken[d.Hostname] })
	return blocked, nil
}
