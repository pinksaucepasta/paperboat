package app

import (
	"context"
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
	// reconciled is each name's last successfully published set; a name is published again only when its set
	// changes or after edgeWildcardRefresh, so the provider is not rewritten every tick.
	reconciled map[string]wildcardPublication
}

// newEdgeWildcards is nil unless Certificates.ManageEdgeWildcards is set.
func newEdgeWildcards(database *db.DB, cfg config.Config, public, runtime wildcardPublisher) *edgeWildcards {
	if !cfg.Certificates.ManageEdgeWildcards {
		return nil
	}
	return &edgeWildcards{db: database, log: slog.Default(), now: time.Now, names: []wildcardName{
		{"*." + cfg.Preview.BaseDomain, public},
		{"*." + cfg.RuntimeBaseDomain, runtime},
	}}
}

// readyEdgeAddresses lists the verified public ingress addresses of managed edges that serve: ready, not draining,
// fresh heartbeat, unexpired registration, in an enabled region. The ingress probe checks every address it records
// (IPv4 and IPv6), so an IPv6 address here has been reached.
func (w *edgeWildcards) readyEdgeAddresses(ctx context.Context) ([]string, error) {
	rows, err := w.db.Pool().Query(ctx, `SELECT coalesce(host(n.public_ingress_ipv4),''),coalesce(host(n.public_ingress_ipv6),'')
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
	var addresses []string
	for rows.Next() {
		var v4, v6 string
		if err = rows.Scan(&v4, &v6); err != nil {
			return nil, err
		}
		for _, address := range []string{v4, v6} {
			if address != "" {
				addresses = append(addresses, address)
			}
		}
	}
	return addresses, rows.Err()
}

// Sync publishes every wildcard name whose set changed (or is due for a refresh). It never publishes an empty
// set: with no ready edge the old records stay, which beats resolving to nothing. A name's failure does not stop the others.
func (w *edgeWildcards) Sync(ctx context.Context) error {
	addresses, err := w.readyEdgeAddresses(ctx)
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		w.log.WarnContext(ctx, "edge_wildcard_dns_empty", "detail", "no ready managed edge; keeping the published records")
		return nil
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
		switch {
		case err == nil:
			if w.reconciled == nil {
				w.reconciled = map[string]wildcardPublication{}
			}
			w.reconciled[name.hostname] = wildcardPublication{addresses: key, at: at}
			w.log.InfoContext(ctx, "edge_wildcard_dns_published", "name", name.hostname, "addresses", key)
		case tunnelcert.WaitingForDNS(err):
			delete(w.reconciled, name.hostname) // retried next tick until DNS agrees
		default:
			delete(w.reconciled, name.hostname)
			w.log.WarnContext(ctx, "edge_wildcard_dns_not_reconciled", "name", name.hostname, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
