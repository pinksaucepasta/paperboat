package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
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

// previewAddressPublisher writes a managed preview's DNS record as soon as an edge holds one of its
// carrier attachments, so the preview never waits for the periodic reconcile to create it.
type previewAddressPublisher struct {
	database   *db.DB
	reconciler *tunnelcert.AddressReconciler
}

func newPreviewAddressPublisher(database *db.DB, cfg config.Config) (*previewAddressPublisher, error) {
	reconciler, err := newEdgeDNSReconciler(database, cfg, cfg.Certificates.PublicDNSZoneID)
	if err != nil {
		return nil, err
	}
	return &previewAddressPublisher{database: database, reconciler: reconciler}, nil
}

var errNoPublishableEdge = errors.New("no edge holds a publishable carrier attachment for the preview yet")

func (p *previewAddressPublisher) PublishPreview(ctx context.Context, previewID string) error {
	var endpoint, mode string
	err := p.database.Pool().QueryRow(ctx, `SELECT p.endpoint,paperboat.workspace_edge_mode(p.account_id,p.owner_team_id) FROM paperboat.preview_leases p WHERE p.id=$1 AND p.terminal_state='active'`, previewID).Scan(&endpoint, &mode)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (mode != "managed-only" || !strings.HasPrefix(endpoint, "https://")) {
		return nil
	}
	if err != nil {
		return err
	}
	host := strings.TrimPrefix(endpoint, "https://")
	rows, err := p.database.Queries().ListReadyEdgeDNSAddressesV1(ctx, dbsqlc.ListReadyEdgeDNSAddressesV1Params{TunnelID: previewID, Hostname: host, Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errNoPublishableEdge
	}
	desired, err := readyEdgePublication(previewID, host, 1, rows)
	if err != nil {
		return err
	}
	return p.reconciler.Publish(ctx, desired)
}

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
					err = reconcileEdgeDNSOwner(ctx, database, public, runtime, owner.ID, uint64(owner.Generation))
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

func reconcileEdgeDNSOwner(ctx context.Context, database *db.DB, public, runtime *tunnelcert.AddressReconciler, owner string, generation uint64) error {
	var runtimeRoute bool
	if err := database.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.control_routes WHERE id=$1 AND kind='runtime_https_wss')`, owner).Scan(&runtimeRoute); err != nil {
		return err
	}
	reconciler := public
	if runtimeRoute {
		reconciler = runtime
	}
	names, err := database.Queries().ListEdgeDNSPublicationNamesV1(ctx, owner)
	if err != nil {
		return err
	}
	owned, err := reconciler.OwnedHostnames(ctx, owner)
	if err != nil {
		return err
	}
	active, existing := map[string]bool{}, map[string]bool{}
	for _, host := range names {
		active[host] = true
	}
	for _, host := range owned {
		existing[host] = true
		if !active[host] {
			names = append(names, host)
		}
	}
	if len(names) > 128 {
		return errors.New("edge DNS hostname bound exceeded")
	}
	sort.Strings(names)
	for _, host := range names {
		desired := tunnelcert.DesiredAddressPublication{Owner: owner, Hostname: host, ResourceGeneration: generation, ObservedAt: time.Now().UTC()}
		if active[host] {
			if runtimeRoute {
				row, queryErr := database.Queries().GetReadyRuntimeEdgeDNSAddressV1(ctx, dbsqlc.GetReadyRuntimeEdgeDNSAddressV1Params{RouteID: owner, Hostname: host})
				if queryErr != nil && queryErr != pgx.ErrNoRows {
					return queryErr
				}
				if queryErr == nil {
					if row.ResourceGeneration != int64(generation) {
						return errors.New("runtime DNS route changed during projection")
					}
					desired.ObservedAt, desired.ValidUntil = row.ObservedAt, row.ValidUntil
					desired.ReadinessVersion = row.ID + "/" + row.ReadinessVersion
					if row.Ipv4 != "" {
						desired.Addresses = append(desired.Addresses, row.Ipv4)
					}
					if row.Ipv6 != "" {
						desired.Addresses = append(desired.Addresses, row.Ipv6)
						desired.IPv6ReachabilityVerified = true
					}
				}
			} else {
				rows, queryErr := database.Queries().ListReadyEdgeDNSAddressesV1(ctx, dbsqlc.ListReadyEdgeDNSAddressesV1Params{TunnelID: owner, Hostname: host, Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
				if queryErr != nil {
					return queryErr
				}
				desired, queryErr = readyEdgePublication(owner, host, generation, rows)
				if queryErr != nil {
					return queryErr
				}
			}
		}
		if len(desired.Addresses) == 0 && !existing[host] {
			continue
		}
		if active[host] {
			if _, err := database.Queries().TransferWithdrawnEdgeDNSNameV1(ctx, dbsqlc.TransferWithdrawnEdgeDNSNameV1Params{Hostname: host, OwnerID: owner, ResourceGeneration: int64(generation), ObservedAt: desired.ObservedAt}); err != nil {
				return err
			}
		}
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = reconciler.Reconcile(attempt, desired)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func readyEdgePublication(owner, host string, generation uint64, rows []dbsqlc.ListReadyEdgeDNSAddressesV1Row) (tunnelcert.DesiredAddressPublication, error) {
	d := tunnelcert.DesiredAddressPublication{Owner: owner, Hostname: host, ResourceGeneration: generation, ObservedAt: time.Now().UTC()}
	if len(rows) > 2 {
		return d, errors.New("edge DNS placement exceeds two nodes")
	}
	var versions []string
	for _, row := range rows {
		if row.ResourceGeneration != int64(generation) {
			return d, errors.New("edge DNS resource changed during projection")
		}
		d.ObservedAt = row.ObservedAt
		if row.Ipv4 != "" {
			d.Addresses = append(d.Addresses, row.Ipv4)
		}
		if row.Ipv6 != "" {
			d.Addresses = append(d.Addresses, row.Ipv6)
			d.IPv6ReachabilityVerified = true
		}
		versions = append(versions, row.ID+"/"+row.ReadinessVersion)
		if d.ValidUntil.IsZero() || row.ValidUntil.Before(d.ValidUntil) {
			d.ValidUntil = row.ValidUntil
		}
	}
	sort.Strings(versions)
	sum := sha256.Sum256([]byte(strings.Join(versions, "\n")))
	d.ReadinessVersion = hex.EncodeToString(sum[:])
	return d, nil
}
