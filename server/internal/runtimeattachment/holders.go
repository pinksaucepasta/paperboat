package runtimeattachment

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"github.com/pinksaucepasta/paperboat/shared/strictjson"

	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
)

// holderNodes are the managed edge nodes a holder answer may name: the same population the peer directory lists
// (controlplane edge_peers.go). A self-hosted node is never in it, so managed and self-hosted nodes never see each other.
const holderNodes = `nodes AS MATERIALIZED (
 SELECT n.id, n.process_epoch FROM control_tunnel_nodes n
 JOIN managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
 JOIN operator_regions region ON region.id=m.region_id AND region.enabled
 WHERE n.roles @> ARRAY['edge']::text[] AND n.state IN ('ready','draining'))`

// liveEndpoint is endpoints.liveEndpoint (server/internal/endpoints/endpoints.go): an endpoint that should be served now.
const liveEndpoint = `e.desired_state='active' AND (e.kind<>'preview' OR EXISTS(SELECT 1 FROM preview_leases p
WHERE p.id=e.preview_lease_id AND p.terminal_state='active' AND p.lease_deadline>now()))
AND (e.kind<>'tunnel' OR EXISTS(SELECT 1 FROM tunnels t WHERE t.id=e.tunnel_id AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())))`

// heldKeys are (holder key, node id) pairs over the managed edge nodes, from the rows Service.Pull (the runtime route of each
// carrier: its hostname) and PullEndpoints (the preview and tunnel routes riding on carriers) read for one node, here for
// all of them (a tunnel route's custom domains too). A key is a hostname, or nodemesh.TCPKey(port) for a public raw TCP tunnel
// route; a private TCP route has none. strict adds what those pulls apply short of carrier validity (the carrier, the
// assignment observed ready, the endpoint live) and the carrier's identity as route and epoch columns, for the caller to check
// runtime_carrier_valid once per carrier; without it the pairs are the cheap superset the key page is cut from.
func heldKeys(strict bool) string {
	runtimeCols, endpointCols, carrier, endpointLive := "", "", "", ""
	if strict {
		runtimeCols, endpointCols = ", a.route_id AS route, a.edge_process_epoch AS epoch", ", a.route_id, a.edge_process_epoch"
		carrier = `JOIN runtime_carrier_attachments a ON a.machine_id=e.agent_machine_id AND a.edge_node_id=x.edge_node_id
 AND a.edge_process_epoch=x.edge_process_epoch AND a.session_id=x.carrier_session_id`
		endpointLive = ` AND x.observed_ready AND ` + liveEndpoint + ` AND (e.kind<>'preview' OR a.agent_capabilities @> ARRAY['endpoints']::text[])`
	}
	return `SELECT lower(a.admission->>'hostname') COLLATE "C" AS key, a.edge_node_id AS node` + runtimeCols + `
 FROM runtime_carrier_attachments a JOIN nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch
 WHERE a.admission->>'hostname' IS NOT NULL
UNION ALL
SELECT names.key COLLATE "C", x.edge_node_id` + endpointCols + `
 FROM endpoint_edge_assignments x JOIN nodes n ON n.id=x.edge_node_id AND n.process_epoch=x.edge_process_epoch
 JOIN endpoint_routes r ON r.id=x.route_id JOIN endpoints e ON e.id=x.endpoint_id
 LEFT JOIN tunnel_routes tr ON tr.id=r.tunnel_route_id ` + carrier + `
 CROSS JOIN LATERAL (
  SELECT CASE WHEN r.protocol='tcp' THEN 'tcp:'||tr.public_tcp_port::text ELSE r.hostname END AS key
  UNION ALL
  -- the custom domains the route is also served under (ingressprojection engineTunnelIngressSQL's second publication)
  SELECT d.hostname FROM tunnel_domains d WHERE tr.protocol IN ('http','tls') AND d.route_id=tr.id AND d.tunnel_id=tr.tunnel_id
   AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear' AND d.match_type='exact'
   AND (tr.protocol='http' OR (d.certificate_strategy='none' AND d.certificate_state='not_applicable'))) names
 WHERE x.desired='active' AND r.protocol<>'private_tcp' AND (r.protocol<>'tcp' OR tr.public_tcp_port IS NOT NULL)` + endpointLive
}

// keyPage is the next limit+1 distinct keys after $1 of the superset: the page's keys, and whether more follow.
var keyPage = `WITH ` + holderNodes + ` SELECT DISTINCT key FROM (` + heldKeys(false) + `) c WHERE key>$1::text ORDER BY key LIMIT $2`

// validHolders groups the keys in ($1, $2] by node once their carrier is valid. runtime_carrier_valid (a plpgsql call of about a
// millisecond) runs once per distinct carrier of the page, never per route: a carrier serves many names (ingressprojection does
// the same). The CTEs are materialized so the planner cannot run the call before the key range cuts the rows.
var validHolders = `WITH ` + holderNodes + `, cand AS MATERIALIZED (SELECT * FROM (` + heldKeys(true) + `) c WHERE key>$1::text AND key<=$2::text),
live_carrier AS MATERIALIZED (SELECT route,node,epoch FROM (SELECT DISTINCT route,node,epoch FROM cand) d WHERE paperboat.runtime_carrier_valid(route,node,epoch,false))
SELECT c.key, array_agg(DISTINCT c.node ORDER BY c.node) FROM cand c JOIN live_carrier v ON v.route=c.route AND v.node=c.node AND v.epoch=c.epoch
GROUP BY c.key ORDER BY c.key`

// Holders answers a managed edge node which nodes hold which keys: one page of at most limit keys after page (the last key of
// the previous page), in key order, with next set when more follow. A caller that is not a managed node gets nothing.
// The page is cut from the structural superset first, so the validity checks (plpgsql calls) run only for the page's keys; a
// page can therefore hold fewer than limit keys, even none, and still have a next.
func (s *Service) Holders(ctx context.Context, callerNode, page string, limit int) (holders []nodemesh.Holder, next string, err error) {
	if limit == 0 {
		limit = nodemesh.DefaultHolderPage
	}
	if limit < 0 || limit > nodemesh.MaxHolderPage || callerNode == "" || (page != "" && nodemesh.ValidKey(page) != nil) {
		return nil, "", ErrInvalid
	}
	var managed bool
	if err = s.DB.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM managed_node_authority WHERE node_id=$1 AND retired_at IS NULL)`, callerNode).Scan(&managed); err != nil || !managed {
		return nil, "", err
	}
	rows, err := s.DB.Pool().Query(ctx, keyPage, page, limit+1)
	if err != nil {
		return nil, "", err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return nil, "", err
		}
		keys = append(keys, key)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(keys) == 0 {
		return nil, "", err
	}
	if len(keys) > limit {
		keys, next = keys[:limit], keys[limit-1]
	}
	rows, err = s.DB.Pool().Query(ctx, validHolders, page, keys[len(keys)-1])
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var h nodemesh.Holder
		if err = rows.Scan(&h.Name, &h.NodeIDs); err != nil {
			return nil, "", err
		}
		// A key the wire cannot carry (a malformed stored hostname) is left out, not allowed to fail the page.
		if h.Validate() == nil {
			holders = append(holders, h)
		}
	}
	return holders, next, rows.Err()
}

// serveHolders is POST nodemesh.HoldersPath: the node bearer is bound to the node and process epoch the body names, as on the
// carrier pull, and the answer is one page of the holder snapshot.
func (h *Handler) serveHolders(w http.ResponseWriter, r *http.Request, raw []byte) {
	var request nodemesh.HoldersRequest
	if strictjson.Decode(raw, &request, 8) != nil || request.Validate() != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	verified := r.Clone(r.Context())
	if len(verified.Header.Values("X-Paperboat-Edge-Process-Epoch")) == 0 {
		verified.Header.Set("X-Paperboat-Edge-Process-Epoch", request.ProcessEpoch)
	}
	edge, err := h.Edge.VerifyPreviewEdgeRequest(r.Context(), verified, raw)
	if err != nil {
		if err == previewattachment.ErrUnauthorized {
			writeError(w, 401, "edge_identity_invalid")
		} else {
			serviceError(w, r, err)
		}
		return
	}
	if edge.NodeID != request.NodeID || edge.ProcessEpoch != request.ProcessEpoch {
		writeError(w, 401, "edge_identity_invalid")
		return
	}
	holders, next, err := h.Service.Holders(r.Context(), edge.NodeID, request.Page, request.PageLimit())
	if err != nil {
		serviceError(w, r, err)
		return
	}
	if holders == nil {
		holders = []nodemesh.Holder{}
	}
	encoded, err := json.Marshal(nodemesh.HoldersResponse{Holders: holders, Next: next})
	if err != nil || len(encoded) > connectorprotocol.MaximumSnapshotBytes {
		serviceError(w, r, ErrUnavailable)
		return
	}
	_, _ = w.Write(encoded)
}
