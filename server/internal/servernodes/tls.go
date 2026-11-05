package servernodes

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// ACMECache is autocert's cache in the shared database, encrypted with the shared key: the nodes of a region
// share one certificate, and any of them answers an HTTP-01 challenge another one started (autocert keeps
// challenge tokens in the cache). Prior art: headscale serves its own TLS with autocert (hscontrol/app.go:1166).
type ACMECache struct {
	DB  *db.DB
	Key string
}

func (c *ACMECache) Get(ctx context.Context, key string) ([]byte, error) {
	var sealed []byte
	err := c.DB.Pool().QueryRow(ctx, `SELECT sealed FROM paperboat.control_server_node_acme_cache WHERE key=$1`, key).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, autocert.ErrCacheMiss
	}
	if err != nil {
		return nil, err
	}
	plain, err := secrets.Decrypt(c.Key, sealed)
	return []byte(plain), err
}

func (c *ACMECache) Put(ctx context.Context, key string, data []byte) error {
	sealed, err := secrets.Encrypt(c.Key, string(data))
	if err != nil {
		return err
	}
	_, err = c.DB.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_node_acme_cache (key, sealed) VALUES ($1,$2)
ON CONFLICT (key) DO UPDATE SET sealed=excluded.sealed, updated_at=now()`, key, sealed)
	return err
}

func (c *ACMECache) Delete(ctx context.Context, key string) error {
	_, err := c.DB.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_node_acme_cache WHERE key=$1`, key)
	return err
}

// RegionalTLS serves handler on :443 for <region>.<domain> with a certificate from ACME (HTTP-01 on :80), and
// turns this node from registered to active once it holds the certificate. directory "" is Let's Encrypt.
type RegionalTLS struct {
	DB                        *db.DB
	Node                      Node
	Domain, Key               string
	Directory                 string
	HTTPSAddress, HTTPAddress string
	Handler                   http.Handler
	ShutdownTimeout           time.Duration // graceful: GOAWAY, then held requests finish
	Logger                    *slog.Logger
}

func (r *RegionalTLS) Name() string { return r.Node.Region + "." + r.Domain }

func (r *RegionalTLS) manager() *autocert.Manager {
	m := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(r.Name()), Cache: &ACMECache{DB: r.DB, Key: r.Key}}
	if r.Directory != "" {
		m.Client = &acme.Client{DirectoryURL: r.Directory}
	}
	return m
}

// Run serves until ctx ends. A listener failure is returned; certificate errors are retried with backoff.
func (r *RegionalTLS) Run(ctx context.Context) error {
	m := r.manager()
	https := &http.Server{Addr: r.HTTPSAddress, Handler: r.Handler, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.GetCertificate, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}}}
	challenge := &http.Server{Addr: r.HTTPAddress, Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 5 * time.Second}
	failed := make(chan error, 2)
	go func() {
		failed <- fmt.Errorf("regional HTTPS on %s: %w", r.HTTPSAddress, https.ListenAndServeTLS("", ""))
	}()
	go func() { failed <- fmt.Errorf("ACME HTTP-01 on %s: %w", r.HTTPAddress, challenge.ListenAndServe()) }()
	go r.activate(ctx, m)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), max(r.ShutdownTimeout, 5*time.Second))
		defer cancel()
		return errors.Join(https.Shutdown(shutdown), challenge.Shutdown(shutdown))
	case err := <-failed:
		_ = https.Close()
		_ = challenge.Close()
		return err
	}
}

// activate obtains the certificate (backoff 5 s to 5 min) and then marks the node active, so it enters the
// region map only once it can serve its name.
func (r *RegionalTLS) activate(ctx context.Context, m *autocert.Manager) {
	delay := 5 * time.Second
	for ctx.Err() == nil {
		_, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: r.Name(), CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
		if err == nil {
			if err = MarkActive(ctx, r.DB, r.Node.ID); err == nil {
				r.Logger.Info("server node serves its regional name", "name", r.Name(), "node", r.Node.ID)
				return
			}
		}
		r.Logger.Warn("server node regional certificate not ready", "name", r.Name(), "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, 5*time.Minute)
	}
}

// MarkActive moves a registered node to active; other states are left as they are.
func MarkActive(ctx context.Context, d *db.DB, id string) error {
	_, err := d.Pool().Exec(ctx, `UPDATE paperboat.control_server_nodes SET state='active' WHERE id=$1 AND state='registered'`, id)
	return err
}
