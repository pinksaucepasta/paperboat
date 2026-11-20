package edgeset

import (
	"fmt"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// ServingNodesPerRegion is how many managed edges of one region serve: a workspace's pair is two regions and a machine
// holds at most runtimewire.MaximumRegionCarriers carriers, so a pair's serving nodes always fit.
const ServingNodesPerRegion = runtimewire.MaximumRegionCarriers / 2

// ServingNodes is the one definition of which managed edges serve, shared by the wildcard DNS names (app
// edge_wildcard_dns.go) and a machine's attach (runtimeattachment, edgeplacement). Edges do not forward, so a visitor
// who resolves a published node must find a carrier there: both take the same first ServingNodesPerRegion live managed
// edges of each region by node id, and the same nodes whichever workspace or machine asks. Columns: id, region, rank
// (1-based within the region). Liveness (ready, not draining, heartbeat, registration, enabled region) is part of the
// definition so a dead node does not hold a place, and so is verified public ingress, so an edge visitors cannot reach
// does not take a verified edge's place; each consumer adds its own further checks after the ranking.
// Prior art: headscale builds one DERP map for every client from the live node list (hscontrol/derp/derp.go).
var ServingNodes = fmt.Sprintf(`(SELECT ranked.id,ranked.region,ranked.rank FROM (
SELECT n.id,n.region,row_number() OVER (PARTITION BY n.region ORDER BY n.id) AS rank
FROM paperboat.control_tunnel_nodes n
JOIN paperboat.managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
JOIN paperboat.operator_regions r ON r.id=m.region_id AND r.enabled
WHERE n.state='ready' AND n.ready AND n.drain_deadline IS NULL AND n.roles @> ARRAY['edge']::text[]
 AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.public_ingress_verified_at IS NOT NULL AND (n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)) ranked
WHERE ranked.rank<=%d)`, ServingNodesPerRegion)

// ServingOnly restricts managed edge n (the caller's alias) to the serving set; other nodes (a workspace's own servers)
// pass. It adds no query parameter.
var ServingOnly = `(NOT EXISTS(SELECT 1 FROM paperboat.managed_node_authority a WHERE a.node_id=n.id) OR n.id IN (SELECT id FROM ` + ServingNodes + ` serving))`
