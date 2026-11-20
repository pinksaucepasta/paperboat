package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// meshFreshness is how recent a node's heartbeat must be for the directory to list it: a few missed heartbeats.
const meshFreshness = 60 * time.Second

var errPeersStale = errors.New("the caller is not the current process of a known node")

// EdgePeers is the peer directory of one managed edge node: the other managed edge nodes (ready or draining, heartbeating,
// with a registered carrier certificate) it may link to for visitor forwarding, each with the pin of the certificate it registered.
// A caller that is not a managed node (a self-hosted edge) gets no peers, and a managed one never lists a self-hosted node:
// the two populations are never mixed. A caller that is not the node's current process gets errPeersStale (the edge re-registers).
func (s *EdgeService) EdgePeers(ctx context.Context, nodeID, processEpoch string) (nodemesh.PeersResponse, error) {
	now := s.clock().UTC()
	out := nodemesh.PeersResponse{Peers: []nodemesh.Peer{}, ObservedAt: now}
	var epoch string
	var managed bool
	err := s.store.SQL().QueryRowContext(ctx, `SELECT n.process_epoch, EXISTS(SELECT 1 FROM managed_node_authority m WHERE m.node_id=n.id AND m.retired_at IS NULL)
FROM control_tunnel_nodes n WHERE n.id=$1 AND n.roles @> ARRAY['edge']::text[]`, nodeID).Scan(&epoch, &managed)
	if errors.Is(err, sql.ErrNoRows) || err == nil && epoch != processEpoch {
		return out, errPeersStale
	}
	if err != nil || !managed {
		return out, err
	}
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT n.id, m.region_id, n.process_epoch, n.carrier_server_spki_sha256, n.state='draining',
 coalesce(CASE WHEN n.public_ingress_verified_at IS NOT NULL THEN coalesce(host(n.public_ingress_ipv4),host(n.public_ingress_ipv6)) END, n.carrier_endpoint_host),
 coalesce(n.public_https_port,443)
FROM control_tunnel_nodes n
JOIN managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
JOIN operator_regions region ON region.id=m.region_id AND region.enabled
WHERE n.roles @> ARRAY['edge']::text[] AND n.state IN ('ready','draining') AND n.id<>$1
 AND n.last_heartbeat_at>$2::timestamptz-make_interval(secs=>$3) AND n.carrier_server_spki_sha256 IS NOT NULL
ORDER BY n.id LIMIT $4`, nodeID, now, meshFreshness.Seconds(), nodemesh.MaxPeers)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p nodemesh.Peer
		var host sql.NullString
		var port int
		if err := rows.Scan(&p.NodeID, &p.Region, &p.ProcessEpoch, &p.SPKISHA256, &p.Draining, &host, &port); err != nil {
			return out, err
		}
		p.Host, p.Port = host.String, uint16(port)
		// A row the wire cannot carry (no address) is left out, not allowed to fail the directory.
		if p.Validate() == nil {
			out.Peers = append(out.Peers, p)
		}
	}
	return out, rows.Err()
}

func (s *EdgeService) handlePeers(w http.ResponseWriter, r *http.Request) {
	var in nodemesh.PeersRequest
	if !s.decode(w, r, &in) {
		return
	}
	if in.Validate() != nil {
		writeEdgeError(w, r, http.StatusBadRequest, "invalid_request", false, 0)
		return
	}
	out, err := s.EdgePeers(r.Context(), in.NodeID, in.ProcessEpoch)
	if errors.Is(err, errPeersStale) {
		// 409 is the re-register directive, as on a heartbeat of a replaced process.
		writeEdgeError(w, r, http.StatusConflict, "node_observation_stale", false, 0, err)
		return
	}
	if err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	}
	writeEdgeJSON(w, http.StatusOK, out)
}
