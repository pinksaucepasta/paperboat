package app

import (
	"context"
	"log/slog"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// One load rule for both kinds of managed node: an overloaded edge leaves the wildcard DNS names (edge_wildcard_dns.go)
// and an overloaded relay leaves the relay candidate list (peersessions relayNodes reads paperboat.node_load_out, and
// within a region lists the least loaded relay first, which is the one the client uses). Decisions are stored in
// paperboat.node_load_out so a worker leader change keeps the hysteresis.

var (
	edgeRoles  = []string{"edge"}
	relayRoles = []string{"relay", "peer_relay"}
)

// loadNode is a ready managed node the load rule decides on: its region and load (nil: unknown).
type loadNode struct {
	id, region string
	load       *float64
}

// loadDecision is a node's stored decision: held (left out of rotation, what readers use) and over (the threshold
// decision with hysteresis, before a region is kept from emptying).
type loadDecision struct{ held, over bool }

// overOf is the previous threshold decision of each stored node.
func overOf(stored map[string]loadDecision) map[string]bool {
	over := make(map[string]bool, len(stored))
	for id, d := range stored {
		over[id] = d.over
	}
	return over
}

// selectByLoad decides, for every node given, whether it is held out of rotation for load, with hysteresis. A node
// whose load reaches outPercent is over; once over it stays so until its load is below inPercent, so nothing between
// the two flaps. A node with unknown load is never over. A region is never left empty: when all its nodes are over,
// the least loaded one (ties by input order, which is by id) is kept in rotation. over is each node's previous
// threshold decision. It returns held (the nodes left out of rotation, with that rule applied) and nextOver (the pure
// threshold decision, WITHOUT the rule): nextOver is what to persist and pass as over next round, so a node kept only
// because its whole region was over is still judged against inPercent, not outPercent, once its region recovers.
// Prior art: Route 53 health-checked records leave an unhealthy record out of an answer but answer with all of them
// when every record of the set is unhealthy (fail open, never an empty answer); tailscale net/netcheck/netcheck.go
// lines 1495-1511 keeps the current preferred DERP region unless another is clearly better (hysteresis).
func selectByLoad(nodes []loadNode, over map[string]bool, outPercent, inPercent float64) (held, nextOver map[string]bool) {
	nextOver = make(map[string]bool, len(nodes))
	held = make(map[string]bool, len(nodes))
	inRegion := map[string]int{}
	for _, n := range nodes {
		threshold := outPercent
		if over[n.id] {
			threshold = inPercent
		}
		nextOver[n.id] = n.load != nil && *n.load >= threshold
		held[n.id] = nextOver[n.id]
		if !held[n.id] {
			inRegion[n.region]++
		}
	}
	least := map[string]*loadNode{}
	for i, n := range nodes {
		if inRegion[n.region] > 0 {
			continue
		}
		if best := least[n.region]; best == nil || *n.load < *best.load {
			least[n.region] = &nodes[i]
		}
	}
	for _, n := range least {
		held[n.id] = false
	}
	return held, nextOver
}

// loadNodeLoadOut reads the stored decisions (node id -> decision) of the nodes with any of roles.
func loadNodeLoadOut(ctx context.Context, database *db.DB, roles []string) (map[string]loadDecision, error) {
	rows, err := database.Pool().Query(ctx, `SELECT l.node_id,l.out_for_load,l.over_threshold FROM paperboat.node_load_out l
JOIN paperboat.control_tunnel_nodes n ON n.id=l.node_id WHERE n.roles && $1::text[]`, roles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	state := map[string]loadDecision{}
	for rows.Next() {
		var id string
		var d loadDecision
		if err = rows.Scan(&id, &d.held, &d.over); err != nil {
			return nil, err
		}
		state[id] = d
	}
	return state, rows.Err()
}

// storeNodeLoadOut writes the decisions that changed and forgets nodes that are no longer ready, so a node that comes
// back later starts fresh.
func storeNodeLoadOut(ctx context.Context, database *db.DB, before map[string]loadDecision, held, over map[string]bool) error {
	var ids []string
	var helds, overs []bool
	for id, h := range held {
		if was, ok := before[id]; !ok || was.held != h || was.over != over[id] {
			ids, helds, overs = append(ids, id), append(helds, h), append(overs, over[id])
		}
	}
	if len(ids) > 0 {
		if _, err := database.Pool().Exec(ctx, `INSERT INTO paperboat.node_load_out(node_id,out_for_load,over_threshold,changed_at) SELECT id,held,over,now() FROM unnest($1::text[],$2::boolean[],$3::boolean[]) AS t(id,held,over)
ON CONFLICT (node_id) DO UPDATE SET out_for_load=excluded.out_for_load,over_threshold=excluded.over_threshold,changed_at=now()`, ids, helds, overs); err != nil {
			return err
		}
	}
	var gone []string
	for id := range before {
		if _, ok := held[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		_, err := database.Pool().Exec(ctx, `DELETE FROM paperboat.node_load_out WHERE node_id=ANY($1::text[])`, gone)
		return err
	}
	return nil
}

// relayLoad decides which managed relays are left out of the relay candidate list. Run by the worker leader only.
type relayLoad struct {
	db                    *db.DB
	outPercent, inPercent float64
}

func newRelayLoad(database *db.DB, cfg config.Config) *relayLoad {
	return &relayLoad{db: database, outPercent: cfg.NodeCapacity.LoadOutPercent, inPercent: cfg.NodeCapacity.LoadInPercent}
}

// readyRelays lists the managed relays the candidate list may name: ready, not draining, fresh heartbeat, unexpired
// registration, in an enabled region. A self-hosted owner's own relays have no managed authority row and never take part.
func (r *relayLoad) readyRelays(ctx context.Context) ([]loadNode, error) {
	rows, err := r.db.Pool().Query(ctx, `SELECT n.id,m.region_id,paperboat.node_load_percent(n.id)
FROM paperboat.control_tunnel_nodes n
JOIN paperboat.managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
JOIN paperboat.operator_regions g ON g.id=m.region_id AND g.enabled
WHERE n.roles && $1::text[] AND n.state='ready' AND n.ready AND n.drain_deadline IS NULL
 AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
ORDER BY n.id`, relayRoles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []loadNode
	for rows.Next() {
		var n loadNode
		var load *float32
		if err = rows.Scan(&n.id, &n.region, &load); err != nil {
			return nil, err
		}
		if load != nil {
			v := float64(*load)
			n.load = &v
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// Sync decides every ready managed relay and stores the decisions.
func (r *relayLoad) Sync(ctx context.Context) error {
	nodes, err := r.readyRelays(ctx)
	if err != nil {
		return err
	}
	before, err := loadNodeLoadOut(ctx, r.db, relayRoles)
	if err != nil {
		return err
	}
	held, over := selectByLoad(nodes, overOf(before), r.outPercent, r.inPercent)
	for id, out := range held {
		if out != before[id].held {
			slog.InfoContext(ctx, "relay_load_rotation_changed", "node", id, "out_for_load", out)
		}
	}
	return storeNodeLoadOut(ctx, r.db, before, held, over)
}
