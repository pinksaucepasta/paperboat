package servernodes

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// AddressPublisher is tunnelcert.AddressReconciler: it makes a hostname's A/AAAA records exactly a set.
type AddressPublisher interface {
	Reconcile(context.Context, tunnelcert.DesiredAddressPublication) error
}

// RegionalDNS points <region>.<domain> at the verified public addresses of the region's active, fresh nodes
// (the address the enrolling node dialed, pinned). Run by the worker leader only, so there is one writer; a
// region whose nodes all left gets an empty set, which removes its records.
type RegionalDNS struct {
	DB        *db.DB
	Publisher AddressPublisher
	Domain    string
	Now       func() time.Time
}

// Sync reconciles every region that has a node row. It keeps going past one region's failure.
func (r *RegionalDNS) Sync(ctx context.Context) error {
	rows, err := r.DB.Pool().Query(ctx, `SELECT region, coalesce(array_agg(endpoint_host ORDER BY endpoint_host)
  FILTER (WHERE state='active' AND endpoint_host IS NOT NULL AND last_seen_at > now() - make_interval(secs => $1)), '{}')
FROM paperboat.control_server_nodes GROUP BY region ORDER BY region`, HealthyWithin.Seconds())
	if err != nil {
		return err
	}
	type region struct {
		id    string
		hosts []string
	}
	var regions []region
	for rows.Next() {
		var g region
		if err := rows.Scan(&g.id, &g.hosts); err != nil {
			rows.Close()
			return err
		}
		regions = append(regions, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	var errs []error
	for _, g := range regions {
		addresses := []string{}
		for _, h := range g.hosts {
			if ip, err := netip.ParseAddr(h); err == nil {
				addresses = append(addresses, ip.Unmap().String())
			}
		}
		at := now().UTC()
		desired := tunnelcert.DesiredAddressPublication{Owner: "server_region:" + g.id, Hostname: g.id + "." + r.Domain,
			ResourceGeneration: uint64(at.Unix()), ObservedAt: at, Addresses: addresses}
		if len(addresses) > 0 {
			desired.ReadinessVersion, desired.ValidUntil = "server-node-heartbeat", at.Add(2*time.Minute)
		}
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.Publisher.Reconcile(attempt, desired)
		cancel()
		if err != nil && !tunnelcert.WaitingForDNS(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
