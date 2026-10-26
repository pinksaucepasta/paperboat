package app

import (
	"context"
	"net"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

// DNS publication is part of the existing server reconciliation lifecycle.
// Deployment-owned verified public address metadata is required before the
// first write. Registration/heartbeats cannot populate that metadata.
func newEdgeDNSReconciler(database *db.DB, cfg config.Config, zoneID string) (*tunnelcert.AddressReconciler, error) {
	admission, err := tunnelcert.NewDatabaseCloudflareAddressAdmission(database, zoneID)
	if err != nil {
		return nil, err
	}
	provider, err := tunnelcert.NewCloudflareDNSProvider(tunnelcert.CloudflareDNSConfig{
		AddressAdmission: admission, ZoneID: zoneID, TokenReference: cfg.Certificates.DNSTokenReference,
		TokenSource: tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv},
		HTTPClient:  providerHTTPClient("cloudflare-edge-dns", 5*time.Second),
		WriteBase:   cfg.Certificates.DNSWriteBase,
	})
	if err != nil {
		return nil, err
	}
	return tunnelcert.NewAddressReconciler(database, provider, net.DefaultResolver)
}

func newEdgeDNSWorker(database *db.DB, cfg config.Config) (func(context.Context) error, error) {
	public, err := newEdgeDNSReconciler(database, cfg, cfg.Certificates.PublicDNSZoneID)
	if err != nil {
		return nil, err
	}
	runtime, err := newEdgeDNSReconciler(database, cfg, cfg.Certificates.DNSZoneID)
	if err != nil {
		return nil, err
	}
	return edgeDNSWorker(database, public, runtime), nil
}

// edgeDNSWorker withdraws the per-hostname DNS records edges used to get, then idles on an empty list: managed
// tunnel, preview and runtime names are covered by DNS-only wildcards on every managed edge (the apex and runtime
// wildcards), and a custom domain's CNAME points at the resource's own managed name, so nothing is
// published any more. Each publication row that still holds records, or is not verified, has its provider records
// deleted by exact provider id and ends up verified with none.
func edgeDNSWorker(database *db.DB, public, runtime *tunnelcert.AddressReconciler) func(context.Context) error {
	return func(ctx context.Context) error {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		cursor := ""
		for {
			finishProgress := workers.BeginIteration(ctx, time.Second)
			cycleErr := func() error {
				rows, err := database.Queries().ListEdgeDNSPublicationResourcesV1(ctx, dbsqlc.ListEdgeDNSPublicationResourcesV1Params{AfterID: cursor, BatchSize: 1})
				if err == nil && len(rows) == 0 {
					cursor = ""
				}
				for _, owner := range rows {
					cursor = owner.ID
					err = withdrawEdgeDNSOwner(ctx, database, public, runtime, owner.ID, uint64(owner.Generation))
					if err != nil {
						break
					}
				}
				if !tunnelcert.WaitingForDNS(err) {
					workers.IterationFailure(ctx, "edge_dns_publication", "edge_dns_reconcile_failed", err)
				}
				return nil
			}()
			finishProgress(cycleErr)
			if cycleErr != nil {
				return cycleErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}

func withdrawEdgeDNSOwner(ctx context.Context, database *db.DB, public, runtime *tunnelcert.AddressReconciler, owner string, generation uint64) error {
	var runtimeRoute bool
	if err := database.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.control_routes WHERE id=$1 AND kind='runtime_https_wss')`, owner).Scan(&runtimeRoute); err != nil {
		return err
	}
	reconciler := public
	if runtimeRoute {
		reconciler = runtime
	}
	hosts, err := reconciler.OwnedHostnames(ctx, owner)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = reconciler.Reconcile(attempt, tunnelcert.DesiredAddressPublication{Owner: owner, Hostname: host, ResourceGeneration: generation, ObservedAt: time.Now().UTC()})
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
