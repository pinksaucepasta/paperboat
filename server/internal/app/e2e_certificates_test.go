package app

import (
	"context"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

// Pebble skips external authorization validation (PEBBLE_VA_ALWAYS_VALID).
// DNS-01 permits the platform wildcard certificates used by managed access;
// HTTP-01 intentionally disables that worker. Everything after the DNS boundary
// is the real ACME issuer, durable coordinator and authenticated edge distribution.
func e2eCertificateRuntime(t *testing.T, database *db.DB, cfg config.Config) *tunnelv1.CertificateRuntime {
	t.Helper()
	secrets := tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv}
	issuerConfig := acmeIssuerConfig(cfg, secrets)
	issuerConfig.DNS = e2ePebbleDNS{}
	issuer, err := tunnelcert.NewACMEIssuer(issuerConfig)
	if err != nil {
		t.Fatal(err)
	}
	caa, err := tunnelcert.NewDNSCAAInspector(tunnelcert.DNSCAAInspectorConfig{Server: cfg.Certificates.CAAResolver, Timeout: cfg.HTTP.RequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	edges, err := tunnelv1.NewSQLPlatformEdgeTargetResolver(database, controlplane.ControlTunnelNodeStaleAfter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Certificates
	runtime, err := tunnelv1.NewCertificateRuntime(tunnelv1.CertificateRuntimeConfig{
		Database: database, Issuer: issuer, CAA: caa, Keys: secrets,
		NodeCredentials:    nodeauth.Composite{operatorinfra.NewService(database, cfg.Secrets.EncryptionKey), selfhost.New(database, cfg.Secrets.EncryptionKey)},
		MasterKeyReference: c.MasterKeyReference, DistributionCredential: []byte(cfg.Secrets.EdgeControlCredential),
		IssuerName: c.Issuer, OwnerID: c.OwnerID, RenewBefore: c.RenewBefore, LockTTL: c.LockTTL,
		DistributionTimeout: c.DistributionTimeout, ExpiryAlertWindow: c.ExpiryAlertWindow, MaxCertificateLifetime: c.MaxCertificateLifetime,
		PlatformBases:       tunnelcert.PlatformCertificateBases{PreviewBaseDomain: cfg.Preview.BaseDomain, TunnelBaseDomain: cfg.Tunnel.BaseDomain, RuntimeBaseDomain: cfg.RuntimeBaseDomain},
		PlatformEdgeTargets: edges.Resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

type e2ePebbleDNS struct{}

func (e2ePebbleDNS) Present(ctx context.Context, record tunnelcert.DNS01Record) (tunnelcert.DNS01Record, error) {
	record.ProviderID = record.Name
	return record, ctx.Err()
}
func (e2ePebbleDNS) Wait(ctx context.Context, _ tunnelcert.DNS01Record) error    { return ctx.Err() }
func (e2ePebbleDNS) Cleanup(ctx context.Context, _ tunnelcert.DNS01Record) error { return ctx.Err() }
