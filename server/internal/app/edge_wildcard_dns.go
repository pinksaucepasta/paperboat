package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// Edges do not forward, so a visitor who resolves a published node must find a carrier there. The preview and runtime
// wildcard names list the serving managed edges (edgeset.ServingNodes: the first few live nodes of each enabled
// region by id), which are exactly the nodes a machine of any workspace holds carriers to inside its two regions. A
// draining node leaves the set at once; visitors holding its address still reach a carrier until the record's TTL
// passes (nodedrain.MoveGrace). Prior art: headscale's DERP map is rebuilt from the live node list on every change
// (hscontrol/derp/derp.go).
const (
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
	// outPercent and inPercent are config.NodeCapacity.LoadOutPercent / LoadInPercent.
	outPercent, inPercent float64
	// maxNodes is config.NodeCapacity.EdgeWildcardMaxNodes; truncatedAt is the ready count last logged as cut by it.
	maxNodes, truncatedAt int
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
		outPercent: cfg.NodeCapacity.LoadOutPercent, inPercent: cfg.NodeCapacity.LoadInPercent, maxNodes: cfg.NodeCapacity.EdgeWildcardMaxNodes,
		names: []wildcardName{{"*." + cfg.Preview.BaseDomain, public}, {"*." + cfg.RuntimeBaseDomain, runtime}}}
}

// wildcardNode is one ready managed edge: its verified public ingress addresses, region and load (nil: unknown).
type wildcardNode struct {
	loadNode
	addresses []string
}

// readyEdges lists the serving managed edges (edgeset.ServingNodes: ready, not draining, fresh heartbeat, unexpired
// registration, enabled region) whose ingress is verified. The ingress probe checks every address it records (IPv4
// and IPv6), so an IPv6 address here has been reached. Nodes come round-robin across regions (every region's first
// node, then every region's second, ...), so the cap on the answer cuts the last of each region's rank, never a
// region; ties of a rank go by region.
func (w *edgeWildcards) readyEdges(ctx context.Context) ([]wildcardNode, error) {
	rows, err := w.db.Pool().Query(ctx, `SELECT n.id,m.region_id,coalesce(host(n.public_ingress_ipv4),''),coalesce(host(n.public_ingress_ipv6),''),paperboat.node_load_percent(n.id)
FROM paperboat.control_tunnel_nodes n
JOIN `+edgeset.ServingNodes+` s ON s.id=n.id
JOIN paperboat.managed_node_authority m ON m.node_id=n.id
WHERE n.public_ingress_verified_at IS NOT NULL AND (n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
ORDER BY s.rank,m.region_id,n.id`)
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
	if err = rows.Err(); err != nil {
		return nil, err
	}
	total := len(nodes)
	if total > w.maxNodes {
		nodes = nodes[:w.maxNodes]
		if w.truncatedAt != total {
			w.log.WarnContext(ctx, "edge_wildcard_dns_truncated", "ready", total, "published", w.maxNodes, "detail", "raise node_capacity.edge_wildcard_max_nodes")
		}
		w.truncatedAt = total
	} else {
		w.truncatedAt = 0
	}
	return nodes, nil
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
	before, err := loadNodeLoadOut(ctx, w.db, edgeRoles)
	if err != nil {
		return err
	}
	loads := make([]loadNode, len(nodes))
	for i, n := range nodes {
		loads[i] = n.loadNode
	}
	held, over := selectByLoad(loads, overOf(before), w.outPercent, w.inPercent)
	if err = storeNodeLoadOut(ctx, w.db, before, held, over); err != nil {
		return err
	}
	var addresses []string
	for _, n := range nodes {
		if !held[n.id] {
			addresses = append(addresses, n.addresses...)
		}
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
