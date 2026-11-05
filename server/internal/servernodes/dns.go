package servernodes

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// AddressPublisher is tunnelcert.AddressReconciler: it makes a hostname's A/AAAA records exactly a set.
type AddressPublisher interface {
	Reconcile(context.Context, tunnelcert.DesiredAddressPublication) error
	OwnedHostnames(context.Context, string) ([]string, error)
}

// RegionalDNS points <region>.<domain> at the public addresses of the region's active, fresh nodes (declared by
// the node in its signed inspection and approved by the enrolling operator). Run by the worker leader only, so there is one writer; a
// region whose nodes all left gets an empty set, which removes its records.
type RegionalDNS struct {
	DB        *db.DB
	Publisher AddressPublisher
	Domain    string
	Now       func() time.Time
	// Issuer and Key, when set, also keep the regional wildcard certificate issued (EnsureCertificate); a failed
	// issuance waits 10 minutes before the next attempt (ACME failure rate limits).
	Issuer       DNS01Issuer
	Key          string
	Logger       *slog.Logger
	issueBackoff time.Time
	// reconciled is each region's last successfully published address set and when; a region is published again
	// only when its set changes or after regionalRefresh, so the provider is not rewritten every tick.
	reconciled map[string]regionPublication
}

type regionPublication struct {
	addresses string
	at        time.Time
}

const regionalRefresh = 5 * time.Minute

// Sync keeps the regional certificate issued and reconciles every region that has a node row. It keeps going
// past one region's failure.
func (r *RegionalDNS) Sync(ctx context.Context) error {
	var certErr error
	if r.Issuer != nil && time.Now().After(r.issueBackoff) {
		if certErr = EnsureCertificate(ctx, r.DB, r.Key, r.Domain, r.Issuer, time.Now()); certErr != nil {
			r.issueBackoff = time.Now().Add(10 * time.Minute)
			if r.Logger != nil {
				r.Logger.Warn("regional certificate not issued; retrying in 10 minutes", "name", wildcardName(r.Domain), "error", certErr)
			}
		}
	}
	rows, err := r.DB.Pool().Query(ctx, `SELECT region, coalesce(array_agg(coalesce(public_address, endpoint_host) ORDER BY coalesce(public_address, endpoint_host))
  FILTER (WHERE state='active' AND coalesce(public_address, endpoint_host) IS NOT NULL
    AND last_seen_at > now() - make_interval(secs => $1)), '{}')
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
		owner := "server_region:" + g.id
		if len(addresses) == 0 {
			// Nothing to withdraw for a region that never published: skip the provider and resolver round trips.
			published, err := r.Publisher.OwnedHostnames(ctx, owner)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if len(published) == 0 {
				continue
			}
		}
		at := now().UTC()
		key := strings.Join(addresses, ",")
		if last, ok := r.reconciled[g.id]; ok && last.addresses == key && at.Sub(last.at) < regionalRefresh {
			continue
		}
		desired := tunnelcert.DesiredAddressPublication{Owner: owner, Hostname: g.id + "." + r.Domain,
			ResourceGeneration: uint64(at.Unix()), ObservedAt: at, Addresses: addresses}
		if len(addresses) > 0 {
			desired.ReadinessVersion, desired.ValidUntil = "server-node-heartbeat", at.Add(2*regionalRefresh)
		}
		attempt, cancel := context.WithTimeout(ctx, time.Minute)
		err := r.Publisher.Reconcile(attempt, desired)
		cancel()
		switch {
		case err == nil:
			if r.reconciled == nil {
				r.reconciled = map[string]regionPublication{}
			}
			r.reconciled[g.id] = regionPublication{addresses: key, at: at}
			if r.Logger != nil {
				r.Logger.Info("regional DNS published", "name", desired.Hostname, "addresses", key, "took", time.Since(at).Round(time.Millisecond))
			}
		case !tunnelcert.WaitingForDNS(err):
			delete(r.reconciled, g.id)
			errs = append(errs, err)
			if r.Logger != nil {
				r.Logger.Warn("regional DNS not reconciled", "name", desired.Hostname, "error", err)
			}
		default:
			delete(r.reconciled, g.id)
		}
	}
	return errors.Join(append(errs, certErr)...)
}
