//go:build linux || darwin || windows

package deviceguard

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"path/filepath"
	"strings"
	"time"
)

type cachedCertificate struct {
	bundle  *CertificateBundle
	renewAt time.Time
}

func (g *guardServer) certificate(ctx context.Context, conn controlConn, owner, hostname string) (*CertificateBundle, error) {
	if !validName(hostname) {
		return nil, errors.New("invalid private certificate name")
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) < 1 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return nil, errors.New("invalid private certificate name")
		}
		for _, r := range label {
			if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return nil, errors.New("invalid private certificate name")
			}
		}
	}
	// Serializing CA creation/trust avoids concurrent root replacement, without
	// holding the registry lock during cryptography or OS trust commands.
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
	base := g.certificateBase(conn, hostname)
	if base == "" {
		g.mu.Unlock()
		return nil, errors.New("certificate requires an owned protected HTTPS listener")
	}
	if g.certificates == nil {
		g.certificates = map[controlConn]map[string]cachedCertificate{}
	}
	if g.certificates[conn] == nil {
		g.certificates[conn] = map[string]cachedCertificate{}
	}
	if cached, ok := g.certificates[conn][hostname]; ok {
		if time.Now().Before(cached.renewAt) {
			g.mu.Unlock()
			return cached.bundle, nil
		}
		delete(g.certificates[conn], hostname)
	}
	if len(g.certificates[conn]) >= 128 {
		g.mu.Unlock()
		return nil, errors.New("private certificate limit reached; restart device access to renew names")
	}
	g.mu.Unlock()
	labels := strings.Split(base, ".")
	suffix, err := splitdns.ValidateSuffix(labels[len(labels)-1])
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(g.cfg.StateDir, "certificates", suffix)
	if err := protectedDirectory(directory, 0700); err != nil {
		return nil, err
	}
	caLock, err := lockState(filepath.Join(directory, "lock"))
	if err != nil {
		return nil, fmt.Errorf("lock private certificate setup (retry if another setup is active): %w", err)
	}
	defer caLock.Close()
	ca, err := splitdns.LoadOrCreateConstrainedCA(directory, suffix)
	if err != nil {
		return nil, err
	}
	if err = installCATrust(ctx, owner, suffix, ca.CertPEM()); err != nil {
		return nil, err
	}
	certificate, key, err := ca.IssueCertificate([]string{hostname})
	if err != nil {
		return nil, err
	}
	bundle := &CertificateBundle{CertificatePEM: certificate, PrivateKeyPEM: key, RootCAPEM: ca.CertPEM()}
	renewAt, err := certificateRenewAt(bundle, time.Now())
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g.certificateBase(conn, hostname) != base {
		return nil, errors.New("protected HTTPS listener was withdrawn during certificate setup")
	}
	if g.certificates[conn] == nil {
		g.certificates[conn] = map[string]cachedCertificate{}
	}
	g.certificates[conn][hostname] = cachedCertificate{bundle: bundle, renewAt: renewAt}
	return bundle, nil
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

// certificateBase requires the registry lock.
func (g *guardServer) certificateBase(conn controlConn, hostname string) string {
	if g.closing {
		return ""
	}
	for _, lease := range g.leases {
		if lease.owner == conn && lease.port == 443 && g.leaseServesName(conn, lease, hostname) {
			return lease.hostname
		}
	}
	return ""
}
