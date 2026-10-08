//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"os"
	"path/filepath"
	"time"
)

type cachedCertificate struct {
	bundle  *CertificateBundle
	renewAt time.Time
}

func (g *guardServer) certificate(ctx context.Context, conn controlConn, owner, hostname string) (*CertificateBundle, error) {
	domain, err := selectedBrowserDomain(g.cfg.StateDir, owner)
	if err != nil {
		return nil, err
	}
	alias, _, err := splitdns.ParseBrowserHostname(hostname, domain)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.certificateGate == nil {
		g.certificateGate = make(chan struct{}, 1)
	}
	gate := g.certificateGate
	g.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-gate }()
	g.mu.Lock()
	if !g.certificateRequestAuthorizedDomain(conn, owner, hostname, alias, domain) {
		g.mu.Unlock()
		return nil, errors.New("certificate requires an owned protected HTTPS gateway and current alias")
	}
	if g.certificates == nil {
		g.certificates = map[controlConn]map[string]cachedCertificate{}
	}
	if g.certificates[conn] == nil {
		g.certificates[conn] = map[string]cachedCertificate{}
	}
	for name := range g.certificates[conn] {
		if !g.certificateAuthorizedKey(conn, owner, name) {
			delete(g.certificates[conn], name)
		}
	}
	cacheKey := alias + "." + domain
	if cached, ok := g.certificates[conn][cacheKey]; ok && time.Now().Before(cached.renewAt) {
		g.mu.Unlock()
		return cached.bundle, nil
	}
	if len(g.certificates[conn]) >= MaxBrowserAliases {
		g.mu.Unlock()
		return nil, errors.New("local browser certificate capacity reached")
	}
	ca := g.ca
	g.mu.Unlock()
	if domain != splitdns.BrowserSuffix {
		ca, err = prepareNamespaceCA(g.cfg.StateDir, domain, true)
		if err != nil {
			return nil, err
		}
	}
	if ca == nil {
		return nil, errors.New("local browser CA is not ready; retry Paperboat installation")
	}
	certificate, key, err := ca.IssueCertificate([]string{"*." + domain, "*." + alias + "." + domain})
	if err != nil {
		return nil, err
	}
	crl, err := ca.RevocationList(time.Now())
	if err != nil {
		return nil, err
	}
	bundle := &CertificateBundle{CertificatePEM: certificate, PrivateKeyPEM: key, RootCAPEM: ca.CertPEM(), RevocationListDER: crl}
	renewAt, err := certificateRenewAt(bundle, time.Now())
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !g.certificateRequestAuthorizedDomain(conn, owner, hostname, alias, domain) {
		return nil, errors.New("protected browser alias was withdrawn during signing")
	}
	if g.certificates[conn] == nil {
		g.certificates[conn] = map[string]cachedCertificate{}
	}
	g.certificates[conn][cacheKey] = cachedCertificate{bundle: bundle, renewAt: renewAt}
	return bundle, nil
}
func (g *guardServer) certificateRequestAuthorizedDomain(conn controlConn, owner, hostname, alias, domain string) bool {
	selected, err := selectedBrowserDomain(g.cfg.StateDir, owner)
	if err != nil || selected != domain || !g.certificateAuthorizedDomain(conn, owner, alias, domain) {
		return false
	}
	_, label, err := splitdns.ParseBrowserHostname(hostname, domain)
	if err != nil {
		return false
	}
	if label == "" {
		return true
	}
	return browserAliasTarget(g.aliases[conn], hostname, domain) != ""
}
func (g *guardServer) certificateAuthorizedKey(conn controlConn, owner, key string) bool {
	domain, err := selectedBrowserDomain(g.cfg.StateDir, owner)
	if err != nil {
		return false
	}
	alias, label, err := splitdns.ParseBrowserHostname(key, domain)
	return err == nil && label == "" && g.certificateAuthorizedDomain(conn, owner, alias, domain)
}

// Registry lock held. Trust never authorizes a requested machine or service.
func (g *guardServer) certificateAuthorizedDomain(conn controlConn, owner, alias, domain string) bool {
	selected, err := selectedBrowserDomain(g.cfg.StateDir, owner)
	if err != nil || selected != domain || g.closing || g.connections[conn] != owner {
		return false
	}
	gateway := false
	for _, lease := range g.leases {
		if lease.owner == conn && lease.uid == owner && lease.hostname == splitdns.BrowserGatewayHostname && lease.ip == splitdns.BrowserGatewayIP && lease.port == 443 {
			gateway = true
			break
		}
	}
	if !gateway {
		return false
	}
	for name, base := range g.aliases[conn] {
		machine, label, e := splitdns.ParseBrowserHostname(name, domain)
		if e == nil && machine == alias && numericBrowserLabel(label) && base == splitdns.BrowserGatewayHostname {
			return true
		}
	}
	return false
}
func prepareNamespaceCA(state, domain string, existingOnly bool) (*splitdns.CA, error) {
	directory := filepath.Join(state, "certificates", domain)
	if existingOnly {
		if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("approved browser CA is missing; reinstall Paperboat")
		}
	}
	if err := protectedDirectory(directory, 0700); err != nil {
		return nil, err
	}
	for _, name := range []string{"rootCA.pem", "rootCA-key.pem", "rootCA.crl"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && !existingOnly {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("unsafe local browser CA state; preserved")
		}
		if err := validateOwnedRootStateFile(path, name, info); err != nil {
			return nil, err
		}
	}
	var ca *splitdns.CA
	var err error
	if existingOnly {
		ca, err = splitdns.LoadConstrainedCA(directory, domain)
	} else {
		ca, err = splitdns.LoadOrCreateConstrainedCA(directory, domain)
	}
	if err != nil {
		return nil, err
	}
	if _, err := ca.RevocationList(time.Now()); err != nil {
		return nil, err
	}
	return ca, nil
}
func (g *guardServer) prepareLocalCA(ctx context.Context) error {
	approved, err := approvedBrowserDomains(g.cfg.StateDir)
	if err != nil {
		return err
	}
	ca, err := prepareNamespaceCA(g.cfg.StateDir, splitdns.BrowserSuffix, false)
	if err != nil {
		return err
	}
	if err := cleanupHistoricalTrustExcept(ctx, g.cfg, ca.CertPEM()); err != nil {
		return err
	}
	for domain := range approved {
		active := ca
		if domain != splitdns.BrowserSuffix {
			active, err = prepareNamespaceCA(g.cfg.StateDir, domain, false)
			if err != nil {
				return err
			}
		}
		if err := installLocalTrust(ctx, g.cfg, active.CertPEM()); err != nil {
			return err
		}
	}
	g.ca = ca
	g.trustReady = true
	return nil
}

func certificateRenewAt(bundle *CertificateBundle, now time.Time) (time.Time, error) {
	leafBlock, _ := pem.Decode(bundle.CertificatePEM)
	rootBlock, _ := pem.Decode(bundle.RootCAPEM)
	if leafBlock == nil || leafBlock.Type != "CERTIFICATE" || rootBlock == nil || rootBlock.Type != "CERTIFICATE" {
		return time.Time{}, errors.New("issued private certificate chain is invalid")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse issued private certificate: %w", err)
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil || root.CheckSignatureFrom(root) != nil || leaf.CheckSignatureFrom(root) != nil {
		return time.Time{}, errors.New("issued private certificate chain is invalid")
	}
	renewAt := now.Add(24 * time.Hour)
	if len(bundle.RevocationListDER) > 0 {
		list, err := splitdns.ValidateCRL(root.Raw, bundle.RevocationListDER, now)
		if err != nil {
			return time.Time{}, err
		}
		renewAt = list.NextUpdate.Add(-time.Hour)
	}
	for _, expires := range []time.Time{leaf.NotAfter, root.NotAfter} {
		candidate := expires.Add(-time.Hour)
		if candidate.Before(renewAt) {
			renewAt = candidate
		}
	}
	if !now.Before(renewAt) {
		return time.Time{}, errors.New("issued private certificate chain expires too soon to cache")
	}
	return renewAt, nil
}

// Registry lock held; stale aliases cannot consume the per-connection leaf bound.
func (g *guardServer) pruneCertificates(conn controlConn, owner string) {
	for alias := range g.certificates[conn] {
		if !g.certificateAuthorizedKey(conn, owner, alias) {
			delete(g.certificates[conn], alias)
		}
	}
}

func prepareInstalledLocalCA(ctx context.Context, cfg Config) error {
	if err := requireInstallerPrivilege(); err != nil {
		return err
	}
	if err := protectedDirectory(cfg.StateDir, 0700); err != nil {
		return err
	}
	lock, err := lockStoppedGuard(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	g := &guardServer{cfg: cfg}
	if err := g.prepareLocalCA(ctx); err != nil {
		return fmt.Errorf("prepare local browser trust; retry Paperboat installation: %w", err)
	}
	return nil
}
