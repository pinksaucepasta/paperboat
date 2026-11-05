package servernodes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

type fakeIssuer struct {
	calls    int
	lifetime time.Duration
}

func (f *fakeIssuer) IssueDNS01(_ context.Context, d tunnelcert.Domain) (tunnelcert.CertificateBundle, error) {
	if err := d.Validate(); err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	f.calls++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	notAfter := time.Now().Add(f.lifetime)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(f.calls)), Subject: pkix.Name{CommonName: d.Hostname}, DNSNames: []string{d.Hostname},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: notAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	return tunnelcert.CertificateBundle{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), NotAfter: notAfter}, nil
}

// The leader issues one wildcard for every region (a valid platform DNS-01 domain), stores it encrypted, and
// reissues only when it nears expiry; a node loads it for its own region name and turns active only from
// registered.
func TestRegionalCertificateIssueStoreAndServeOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprintf("t%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_node_acme_cache WHERE key=$1`, certificateKey)
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE region=$1`, p)
	})
	_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_node_acme_cache WHERE key=$1`, certificateKey)
	key, domain := "0123456789abcdef0123456789abcdef", "regions.example.test"
	issuer := &fakeIssuer{lifetime: 90 * 24 * time.Hour}
	if err := EnsureCertificate(ctx, store, key, domain, issuer, time.Now()); err != nil || issuer.calls != 1 {
		t.Fatalf("first issuance: %v calls %d", err, issuer.calls)
	}
	if err := EnsureCertificate(ctx, store, key, domain, issuer, time.Now()); err != nil || issuer.calls != 1 {
		t.Fatalf("a valid certificate was reissued: %v calls %d", err, issuer.calls)
	}
	if err := EnsureCertificate(ctx, store, key, domain, issuer, time.Now().Add(70*24*time.Hour)); err != nil || issuer.calls != 2 {
		t.Fatalf("a certificate near expiry was not renewed: %v calls %d", err, issuer.calls)
	}
	var sealed []byte
	if err := store.Pool().QueryRow(ctx, `SELECT sealed FROM paperboat.control_server_node_acme_cache WHERE key=$1`, certificateKey).Scan(&sealed); err != nil || strings.Contains(string(sealed), "PRIVATE KEY") {
		t.Fatalf("stored in clear: %v", err)
	}

	id := p + "-1"
	if _, err := store.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_nodes (id, region, state, version, commit, started_at, last_seen_at)
VALUES ($1,$2,'registered','v','c',now(),now())`, id, p); err != nil {
		t.Fatal(err)
	}
	node := &RegionalTLS{DB: store, Node: Node{ID: id, Region: p}, Domain: domain, Key: key, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := node.reload(ctx); err != nil {
		t.Fatalf("node could not load its region's certificate: %v", err)
	}
	if c, err := node.getCertificate(nil); err != nil || c.Leaf.VerifyHostname(p+"."+domain) != nil {
		t.Fatalf("served certificate does not cover %s.%s: %v", p, domain, err)
	}
	wrong := &RegionalTLS{DB: store, Node: Node{ID: id, Region: p}, Domain: "other.example.test", Key: key}
	if err := wrong.reload(ctx); err == nil {
		t.Fatal("a certificate for another domain was accepted")
	}
	if err := MarkActive(ctx, store, id); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.Pool().QueryRow(ctx, `SELECT state FROM paperboat.control_server_nodes WHERE id=$1`, id).Scan(&state); err != nil || state != "active" {
		t.Fatalf("state %s %v", state, err)
	}
	if _, err := Drain(ctx, store, id); err != nil {
		t.Fatal(err)
	}
	if err := MarkActive(ctx, store, id); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT state FROM paperboat.control_server_nodes WHERE id=$1`, id).Scan(&state); err != nil || state != "draining" {
		t.Fatalf("activation revived a draining node: %s %v", state, err)
	}
}
