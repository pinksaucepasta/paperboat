package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// Edges do not forward, and a machine holds a carrier to every ready node of its two regions (at most
// runtimewire.MaximumRegionCarriers), so a visitor may land on any managed edge: the preview and runtime wildcard
// names list every ready, non-draining managed edge, and the server keeps them so. A draining node leaves the
// set at once; visitors holding its address still reach a carrier until the record's TTL passes (nodedrain.MoveGrace).
// Prior art: headscale's DERP map is rebuilt from the live node list on every change (hscontrol/derp/derp.go).
const (
	edgeWildcardMaxNodes = 8 // runtimewire.MaximumRegionCarriers
	edgeWildcardRefresh  = 5 * time.Minute
	edgeWildcardValidity = 10 * time.Minute
)

type wildcardPublisher interface {
	Reconcile(context.Context, tunnelcert.DesiredAddressPublication) error
}

type wildcardName struct {
	hostname  string
	publisher wildcardPublisher
}

type wildcardPublication struct {
	addresses string
	at        time.Time
}

// edgeWildcards publishes the wildcard names. Run by the worker leader only, so there is one writer.
type edgeWildcards struct {
	db    *db.DB
	names []wildcardName
	now   func() time.Time
	log   *slog.Logger
	// outPercent and inPercent are config.NodeCapacity.DNSLoadOutPercent / DNSLoadInPercent.
	outPercent, inPercent float64
	// reconciled is each name's last successfully published set; a name is published again only when its set
	// changes or after edgeWildcardRefresh, so the provider is not rewritten every tick.
	reconciled map[string]wildcardPublication
}

// newEdgeWildcards is nil unless Certificates.ManageEdgeWildcards is set.
func newEdgeWildcards(database *db.DB, cfg config.Config, public, runtime wildcardPublisher) *edgeWildcards {
	if !cfg.Certificates.ManageEdgeWildcards {
		return nil
	}
	return &edgeWildcards{db: database, log: slog.Default(), now: time.Now,
		outPercent: cfg.NodeCapacity.DNSLoadOutPercent, inPercent: cfg.NodeCapacity.DNSLoadInPercent,
		names: []wildcardName{{"*." + cfg.Preview.BaseDomain, public}, {"*." + cfg.RuntimeBaseDomain, runtime}}}
}

// wildcardNode is one ready managed edge: its verified public ingress addresses, region and load (nil: unknown).
type wildcardNode struct {
	id, region string
	addresses  []string
	load       *float64
}

// readyEdges lists the managed edges that serve: ready, not draining, fresh heartbeat, unexpired registration, in an
// enabled region, ingress verified. The ingress probe checks every address it records (IPv4 and IPv6), so an IPv6
// address here has been reached.
func (w *edgeWildcards) readyEdges(ctx context.Context) ([]wildcardNode, error) {
	rows, err := w.db.Pool().Query(ctx, `SELECT n.id,m.region_id,coalesce(host(n.public_ingress_ipv4),''),coalesce(host(n.public_ingress_ipv6),''),paperboat.node_load_percent(n.id)
FROM paperboat.control_tunnel_nodes n
JOIN paperboat.managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
JOIN paperboat.operator_regions r ON r.id=m.region_id AND r.enabled
WHERE n.state='ready' AND n.ready AND n.drain_deadline IS NULL AND n.roles @> ARRAY['edge']::text[]
 AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.public_ingress_verified_at IS NOT NULL AND (n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
ORDER BY n.id LIMIT $1`, edgeWildcardMaxNodes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []wildcardNode
	for rows.Next() {
		var n wildcardNode
		var v4, v6 string
		var load *float32
		if err = rows.Scan(&n.id, &n.region, &v4, &v6, &load); err != nil {
			return nil, err
		}
		for _, address := range []string{v4, v6} {
			if address != "" {
				n.addresses = append(n.addresses, address)
			}
		}
		if load != nil {
			v := float64(*load)
			n.load = &v
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// selectWildcardNodes drops overloaded nodes from the published set, with hysteresis. A node whose load reaches
// outPercent leaves; once out it returns only when its load is below inPercent, so nothing between the two flaps.
// A node with unknown load is never dropped. A region is never left empty: when all its ready nodes are out, the
// least loaded one (ties by id) stays. out is each node's previous decision; the result is the kept nodes (in input
// order) and the new decision for every node given.
// Prior art: Route 53 health-checked records leave an unhealthy record out of an answer but answer with all of them
// when every record of the set is unhealthy (fail open, never an empty answer); tailscale net/netcheck/netcheck.go
// lines 1495-1511 keeps the current preferred DERP region unless another is clearly better (hysteresis).
func selectWildcardNodes(nodes []wildcardNode, out map[string]bool, outPercent, inPercent float64) ([]wildcardNode, map[string]bool) {
	state := make(map[string]bool, len(nodes))
	inRegion := map[string]int{}
	for _, n := range nodes {
		threshold := outPercent
		if out[n.id] {
			threshold = inPercent
		}
		state[n.id] = n.load != nil && *n.load >= threshold
		if !state[n.id] {
			inRegion[n.region]++
		}
	}
	least := map[string]*wildcardNode{}
	for i, n := range nodes {
		if inRegion[n.region] > 0 {
			continue
		}
		if best := least[n.region]; best == nil || *n.load < *best.load {
			least[n.region] = &nodes[i]
		}
	}
	for _, n := range least {
		state[n.id] = false
	}
	var kept []wildcardNode
	for _, n := range nodes {
		if !state[n.id] {
			kept = append(kept, n)
		}
	}
	return kept, state
}

// loadWildcardState reads the stored decisions (node id -> held out for load).
func (w *edgeWildcards) loadWildcardState(ctx context.Context) (map[string]bool, error) {
	rows, err := w.db.Pool().Query(ctx, `SELECT node_id,out_for_load FROM paperboat.edge_wildcard_nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	state := map[string]bool{}
	for rows.Next() {
		var id string
		var out bool
		if err = rows.Scan(&id, &out); err != nil {
			return nil, err
		}
		state[id] = out
	}
	return state, rows.Err()
}

// storeWildcardState writes the decisions that changed and forgets nodes that are no longer ready, so a node that
// comes back later starts fresh.
func (w *edgeWildcards) storeWildcardState(ctx context.Context, before, after map[string]bool) error {
	var ids []string
	var outs []bool
	for id, out := range after {
		if was, ok := before[id]; !ok || was != out {
			ids, outs = append(ids, id), append(outs, out)
		}
	}
	if len(ids) > 0 {
		if _, err := w.db.Pool().Exec(ctx, `INSERT INTO paperboat.edge_wildcard_nodes(node_id,out_for_load,changed_at) SELECT id,out,now() FROM unnest($1::text[],$2::boolean[]) AS t(id,out)
ON CONFLICT (node_id) DO UPDATE SET out_for_load=excluded.out_for_load,changed_at=now()`, ids, outs); err != nil {
			return err
		}
	}
	var gone []string
	for id := range before {
		if _, ok := after[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		_, err := w.db.Pool().Exec(ctx, `DELETE FROM paperboat.edge_wildcard_nodes WHERE node_id=ANY($1::text[])`, gone)
		return err
	}
	return nil
}

// Sync publishes every wildcard name whose set changed (or is due for a refresh). It never publishes an empty
// set: with no ready edge the old records stay, which beats resolving to nothing. A name's failure does not stop the others.
func (w *edgeWildcards) Sync(ctx context.Context) error {
	nodes, err := w.readyEdges(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		w.log.WarnContext(ctx, "edge_wildcard_dns_empty", "detail", "no ready managed edge; keeping the published records")
		return nil
	}
	before, err := w.loadWildcardState(ctx)
	if err != nil {
		return err
	}
	kept, after := selectWildcardNodes(nodes, before, w.outPercent, w.inPercent)
	if err = w.storeWildcardState(ctx, before, after); err != nil {
		return err
	}
	var addresses []string
	for _, n := range kept {
		addresses = append(addresses, n.addresses...)
	}
	at, key := w.now().UTC(), strings.Join(addresses, ",")
	var firstErr error
	for _, name := range w.names {
		if last, ok := w.reconciled[name.hostname]; ok && last.addresses == key && at.Sub(last.at) < edgeWildcardRefresh {
			continue
		}
		desired := tunnelcert.DesiredAddressPublication{Owner: "edge_wildcard:" + name.hostname, Hostname: name.hostname,
			ResourceGeneration: uint64(at.Unix()), ReadinessVersion: "edge-heartbeat", ObservedAt: at, ValidUntil: at.Add(edgeWildcardValidity),
			Addresses: addresses, IPv6ReachabilityVerified: true}
		attempt, cancel := context.WithTimeout(ctx, time.Minute)
		err := name.publisher.Reconcile(attempt, desired)
		cancel()
		var published *tunnelcert.AddressPublicationError
		switch {
		case err == nil:
			if w.reconciled == nil {
				w.reconciled = map[string]wildcardPublication{}
			}
			w.reconciled[name.hostname] = wildcardPublication{addresses: key, at: at}
			w.log.InfoContext(ctx, "edge_wildcard_dns_published", "hostname", name.hostname, "addresses", key)
		case tunnelcert.WaitingForDNS(err):
			delete(w.reconciled, name.hostname) // retried next tick until DNS agrees
		default:
			delete(w.reconciled, name.hostname)
			event := "edge_wildcard_dns_not_reconciled"
			if errors.As(err, &published) && published.Kind == tunnelcert.AddressPublicationConflict {
				event = "edge_wildcard_dns_conflict" // a hand-made record for the name: delete it, see the runbook
			}
			w.log.WarnContext(ctx, event, "hostname", name.hostname, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
