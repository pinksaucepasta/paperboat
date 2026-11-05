package servernodes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// One certificate for *.<regional_domain> covers every region name. The worker leader issues and renews it with
// DNS-01 through the existing ACME issuer (one writer, no port 80, one issuance for all regions and nodes) and
// stores it encrypted in the shared database; every node serves it from there and picks up renewals without a
// restart. Prior art: headscale terminates its own TLS in the control server (hscontrol/app.go:1166); Caddy's
// cluster mode shares one certificate store between instances behind one name.

const (
	certificateKey = "regional-wildcard"
	renewBefore    = 30 * 24 * time.Hour
)

// DNS01Issuer is tunnelcert.ACMEIssuer.
type DNS01Issuer interface {
	IssueDNS01(context.Context, tunnelcert.Domain) (tunnelcert.CertificateBundle, error)
}

type storedCertificate struct {
	CertificatePEM []byte    `json:"certificate_pem"`
	PrivateKeyPEM  []byte    `json:"private_key_pem"`
	NotAfter       time.Time `json:"not_after"`
}

func wildcardName(domain string) string { return "*." + domain }

func loadCertificate(ctx context.Context, d *db.DB, key string) (storedCertificate, error) {
	var sealed []byte
	err := d.Pool().QueryRow(ctx, `SELECT sealed FROM paperboat.control_server_node_acme_cache WHERE key=$1`, certificateKey).Scan(&sealed)
	if err != nil {
		return storedCertificate{}, err
	}
	plain, err := secrets.Decrypt(key, sealed)
	if err != nil {
		return storedCertificate{}, err
	}
	var c storedCertificate
	return c, json.Unmarshal([]byte(plain), &c)
}

// EnsureCertificate issues the regional wildcard when none is stored or it expires within 30 days. The leader
// calls it; it returns quickly when nothing is due.
func EnsureCertificate(ctx context.Context, d *db.DB, key, domain string, issuer DNS01Issuer, now time.Time) error {
	current, err := loadCertificate(ctx, d, key)
	if err == nil && current.NotAfter.Sub(now) > renewBefore {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the regional certificate: %w", err)
	}
	bundle, err := issuer.IssueDNS01(ctx, tunnelcert.Domain{ID: "regional_wildcard_v1", AccountID: tunnelcert.PlatformAccountID,
		TargetKind: tunnelcert.TargetPlatformWildcard, Hostname: wildcardName(domain), Generation: 1,
		Strategy: tunnelcert.StrategyPlatformDNS01, OwnershipState: "verified"})
	if err != nil {
		return fmt.Errorf("issue %s: %w", wildcardName(domain), err)
	}
	if _, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM); err != nil {
		return fmt.Errorf("issued %s is unusable: %w", wildcardName(domain), err)
	}
	plain, _ := json.Marshal(storedCertificate{CertificatePEM: bundle.CertificatePEM, PrivateKeyPEM: bundle.PrivateKeyPEM, NotAfter: bundle.NotAfter})
	sealed, err := secrets.Encrypt(key, string(plain))
	if err != nil {
		return err
	}
	_, err = d.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_node_acme_cache (key, sealed) VALUES ($1,$2)
ON CONFLICT (key) DO UPDATE SET sealed=excluded.sealed, updated_at=now()`, certificateKey, sealed)
	return err
}

// RegionalTLS serves handler for <region>.<domain> with the stored wildcard, reloading it every minute, and
// turns this node from registered to active once it can serve.
type RegionalTLS struct {
	DB              *db.DB
	Node            Node
	Domain, Key     string
	Address         string // HTTPS listen address, for example :443
	Handler         http.Handler
	Logger          *slog.Logger
	ShutdownTimeout time.Duration // graceful: GOAWAY, then held requests finish

	mu   sync.RWMutex
	cert *tls.Certificate
}

func (r *RegionalTLS) Name() string { return r.Node.Region + "." + r.Domain }

// reload loads the stored certificate when it changed; it keeps the one it has on a read error.
func (r *RegionalTLS) reload(ctx context.Context) error {
	stored, err := loadCertificate(ctx, r.DB, r.Key)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(stored.CertificatePEM, stored.PrivateKeyPEM)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(r.Name()) != nil {
		return fmt.Errorf("the stored regional certificate does not cover %s", r.Name())
	}
	pair.Leaf = leaf
	r.mu.Lock()
	r.cert = &pair
	r.mu.Unlock()
	return nil
}

func (r *RegionalTLS) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cert == nil {
		return nil, errors.New("regional certificate not loaded yet")
	}
	return r.cert, nil
}

// Run serves until ctx ends: it waits for the certificate (backoff 5 s to 5 min), marks the node active, then
// serves HTTPS and reloads the certificate every minute.
func (r *RegionalTLS) Run(ctx context.Context) error {
	delay := 5 * time.Second
	for {
		err := r.reload(ctx)
		if err == nil {
			break
		}
		r.Logger.Warn("server node regional certificate not ready", "name", r.Name(), "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(2*delay, 5*time.Minute)
	}
	server := &http.Server{Addr: r.Address, Handler: r.Handler, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.getCertificate, NextProtos: []string{"h2", "http/1.1"}}}
	failed := make(chan error, 1)
	go func() { failed <- fmt.Errorf("regional HTTPS on %s: %w", r.Address, server.ListenAndServeTLS("", "")) }()
	if err := MarkActive(ctx, r.DB, r.Node.ID); err != nil {
		r.Logger.Warn("server node could not mark itself active", "error", err)
	} else {
		r.Logger.Info("server node serves its regional name", "name", r.Name(), "node", r.Node.ID)
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), max(r.ShutdownTimeout, 5*time.Second))
			defer cancel()
			return server.Shutdown(shutdown)
		case err := <-failed:
			return err
		case <-ticker.C:
			if err := r.reload(ctx); err != nil {
				r.Logger.Warn("server node kept its regional certificate; reload failed", "error", err)
			}
		}
	}
}

// MarkActive moves a registered node to active; other states are left as they are.
func MarkActive(ctx context.Context, d *db.DB, id string) error {
	_, err := d.Pool().Exec(ctx, `UPDATE paperboat.control_server_nodes SET state='active' WHERE id=$1 AND state='registered'`, id)
	return err
}
