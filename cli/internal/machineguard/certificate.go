//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cachedCertificate struct {
	bundle  *CertificateBundle
	renewAt time.Time
}

func (g *guardServer) certificate(ctx context.Context, conn controlConn, owner, hostname string) (*CertificateBundle, error) {
	domains, err := loadBrowserDomains(g.cfg.StateDir)
	if err != nil {
		return nil, err
	}
	domain, err := domains.selected(owner)
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
	cacheKey := alias + "." + domain
	if cached, ok := g.certificates[conn][cacheKey]; ok && time.Now().Before(cached.renewAt) {
		g.mu.Unlock()
		return cached.bundle, nil
	}
	if len(g.certificates[conn]) >= maxCachedCertificates {
		g.pruneCertificates(conn, owner)
	}
	ca := g.ca
	g.mu.Unlock()
	if domain != domains.Default {
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
	if g.publishCRL != nil {
		if err := g.publishCRL(crl); err != nil {
			slog.Warn("local revocation list was not published; Windows TLS clients need port 80 to fetch it", "error", err)
		}
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
	if _, cached := g.certificates[conn][cacheKey]; cached || len(g.certificates[conn]) < maxCachedCertificates {
		g.certificates[conn][cacheKey] = cachedCertificate{bundle: bundle, renewAt: renewAt}
	}
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

// Registry lock held. Trust never authorizes a requested machine or service.
func (g *guardServer) certificateAuthorizedDomain(conn controlConn, owner, alias, domain string) bool {
	if !g.ownsHTTPSGateway(conn, owner) {
		return false
	}
	for name := range g.aliases[conn] {
		if strings.HasSuffix(name, alias+"."+domain) { // cheap first: this runs per certificate over every alias
			if machine, ok := g.certificateAlias(conn, name, domain); ok && machine == alias {
				return true
			}
		}
	}
	return false
}

// ownsHTTPSGateway requires the registry lock.
func (g *guardServer) ownsHTTPSGateway(conn controlConn, owner string) bool {
	if g.closing || g.connections[conn] != owner {
		return false
	}
	for _, lease := range g.leases {
		if lease.owner == conn && lease.uid == owner && lease.hostname == splitdns.BrowserGatewayHostname && lease.ip == splitdns.BrowserGatewayIP && lease.port == 443 {
			return true
		}
	}
	return false
}

// certificateAlias is the machine (or top-level) label a registered alias authorizes a certificate for.
func (g *guardServer) certificateAlias(conn controlConn, name, domain string) (string, bool) {
	machine, label, err := splitdns.ParseBrowserHostname(name, domain)
	if err != nil {
		return "", false
	}
	numeric := numericBrowserLabel(label) && g.aliases[conn][name] == splitdns.BrowserGatewayHostname
	return machine, numeric || label == "" && browserAliasTarget(g.aliases[conn], name, domain) != ""
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

// retireHistoricalTrust is the platform removal of superseded roots; tests replace it.
var retireHistoricalTrust = cleanupHistoricalTrustExcept

func (g *guardServer) prepareLocalCA(ctx context.Context) error {
	domains, err := loadBrowserDomains(g.cfg.StateDir)
	if err != nil {
		return err
	}
	var ca *splitdns.CA
	var active []byte
	if domains.Default != "" {
		if ca, err = prepareNamespaceCA(g.cfg.StateDir, domains.Default, false); err != nil {
			return err
		}
		active = ca.CertPEM()
	}
	if err := retireHistoricalTrust(ctx, g.cfg, active); err != nil {
		// Retiring older Paperboat roots is housekeeping: failing to remove one (a keychain that refuses,
		// a browser database in use) must not keep the active root untrusted and local access down.
		slog.Warn("older Paperboat local roots could not be retired", "error", err)
	}
	// The CA is complete once generated. Only system trust installation may be
	// pending, so every approved namespace CA is still prepared.
	g.ca = ca
	g.trustReady = false
	var trustErr error
	for domain := range domains.approved() {
		current := ca
		if domain != domains.Default {
			current, err = prepareNamespaceCA(g.cfg.StateDir, domain, false)
			if err != nil {
				return err
			}
		}
		// One failed trust request stops further OS trust prompts for this run.
		if trustErr == nil {
			trustErr = installLocalTrust(ctx, g.cfg, current.CertPEM())
		}
	}
	if trustErr != nil {
		return trustErr
	}
	g.trustReady = domains.Default != ""
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
	if len(g.certificates[conn]) == 0 {
		return
	}
	domain, err := selectedBrowserDomain(g.cfg.StateDir, owner)
	live := map[string]bool{}
	if err == nil && g.ownsHTTPSGateway(conn, owner) {
		for name := range g.aliases[conn] {
			if alias, ok := g.certificateAlias(conn, name, domain); ok {
				live[alias+"."+domain] = true
			}
		}
	}
	for key := range g.certificates[conn] {
		if !live[key] {
			delete(g.certificates[conn], key)
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
	cfg.promptTrust = true
	g := &guardServer{cfg: cfg}
	// Bounded like the guard's own start: an unanswered OS approval prompt leaves trust pending.
	trustCtx, cancel := context.WithTimeout(ctx, guardTrustTimeout)
	defer cancel()
	err = g.prepareLocalCA(trustCtx)
	if err != nil && ctx.Err() != nil { // the caller's own deadline or cancellation is a failure, never pending
		err = fmt.Errorf("%v: %w", err, ctx.Err()) // %v drops ErrTrustPending
	} else if isTrustPending(err) {
		return err
	}
	if err != nil {
		return fmt.Errorf("prepare local browser trust; retry Paperboat installation: %w", err)
	}
	return nil
}
