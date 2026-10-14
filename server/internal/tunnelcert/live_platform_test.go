package tunnelcert_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelcert"
)

// This read-only deployment probe runs in the deployed server container, where
// the configured database, protected key references and OS trust roots exist.
// It never prints certificate material, private keys or configuration values.
func TestLivePlatformCertificateDeploymentProbe(t *testing.T) {
	if os.Getenv("PAPERBOAT_LIVE_PLATFORM_CERTIFICATE_PROBE") != "1" {
		t.Skip("opt-in deployed certificate verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := config.Load(ctx, config.LoadOptions{})
	if err != nil {
		t.Fatal("load deployed configuration failed")
	}
	database, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal("open deployed certificate database failed")
	}
	defer database.Close()
	store, err := tunnelcert.NewPlatformCertificateStore(database)
	if err != nil {
		t.Fatal("open platform certificate store failed")
	}
	definitions, err := tunnelcert.PlatformCertificateTargetDefinitions(tunnelcert.PlatformCertificateBases{
		PreviewBaseDomain: cfg.Preview.BaseDomain, TunnelBaseDomain: cfg.Tunnel.BaseDomain,
		RuntimeBaseDomain: cfg.RuntimeBaseDomain,
	})
	if err != nil || len(definitions) != 2 {
		t.Fatal("configured public/runtime certificate owners are invalid")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal("load deployed public CA trust roots failed")
	}
	keys := tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv}
	for _, target := range definitions {
		current, found, err := store.Current(ctx, target.ID)
		if err != nil || !found || current.State != tunnelcert.StateActive || current.Hostname != target.Hostname {
			t.Fatalf("%s: configured certificate is not active", target.Kind)
		}
		now := time.Now().UTC()
		if !current.RenewalAt.Before(current.ExpiresAt) || !current.RenewalAt.After(now) {
			t.Fatalf("%s: certificate needs renewal or has no bounded renewal schedule", target.Kind)
		}
		bundle, err := tunnelcert.OpenParts(ctx, keys, current.MasterKeyReference, current.CertificateCiphertext, current.PrivateKeyCiphertext)
		if err != nil {
			t.Fatalf("%s: open protected certificate material failed", target.Kind)
		}
		defer clear(bundle.PrivateKeyPEM)
		bundle.Issuer, bundle.NotBefore, bundle.NotAfter = current.Issuer, current.NotBefore, current.ExpiresAt
		host := "certificate-verification." + strings.TrimPrefix(target.Hostname, "*.")
		if _, err := bundle.Validate(host, now, cfg.Certificates.MaxCertificateLifetime); err != nil {
			t.Fatalf("%s: certificate hostname, key or validity verification failed", target.Kind)
		}
		pair, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM)
		if err != nil || len(pair.Certificate) == 0 {
			t.Fatalf("%s: parse certificate chain failed", target.Kind)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatalf("%s: parse certificate leaf failed", target.Kind)
		}
		intermediates := x509.NewCertPool()
		for _, raw := range pair.Certificate[1:] {
			certificate, err := x509.ParseCertificate(raw)
			if err != nil {
				t.Fatalf("%s: parse intermediate certificate failed", target.Kind)
			}
			intermediates.AddCert(certificate)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, CurrentTime: now, Roots: roots, Intermediates: intermediates}); err != nil {
			t.Fatalf("%s: public CA chain verification failed", target.Kind)
		}
		var active int
		err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_certificate_edge_distributions d
JOIN paperboat.control_tunnel_nodes n ON n.id=d.edge_node_id AND n.process_epoch=d.edge_process_epoch
WHERE d.certificate_id=$1 AND d.state='active' AND d.observed_certificate_generation=$2
AND d.edge_assignment_generation>0 AND n.ready AND n.state='ready'
AND n.last_heartbeat_at>now()-interval '2 minutes'`, current.ID, current.CertificateGeneration).Scan(&active)
		if err != nil || active < 1 {
			t.Fatalf("%s: no acknowledged distribution on a current ready edge", target.Kind)
		}
		t.Logf("%s: public CA/key/hostname verified; active edges=%d; expires=%s; renewal=%s", target.Kind, active, current.ExpiresAt.Format(time.RFC3339), current.RenewalAt.Format(time.RFC3339))
	}
}
