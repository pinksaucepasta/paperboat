//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"strings"
)

func parseLocalTrustRoot(certificatePEM []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("invalid Paperboat local CA certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Paperboat local CA certificate: %w", err)
	}
	if !certificate.IsCA || certificate.CheckSignatureFrom(certificate) != nil || !certificate.PermittedDNSDomainsCritical || len(certificate.PermittedDNSDomains) != 1 {
		return nil, errors.New("certificate is not the constrained Paperboat local CA")
	}
	domain := strings.TrimPrefix(certificate.PermittedDNSDomains[0], ".")
	if clean, err := splitdns.NormalizeBrowserDomain(domain); err != nil || clean != domain || certificate.PermittedDNSDomains[0] != "."+domain {
		return nil, errors.New("invalid Paperboat local CA namespace")
	}
	if certificate.Subject.CommonName != "Paperboat Local Root CA" || len(certificate.Subject.Organization) != 1 || certificate.Subject.Organization[0] != "Paperboat Local Development CA" {
		return nil, errors.New("certificate is not owned by Paperboat")
	}
	return certificate, nil
}

func localTrustFingerprint(certificate *x509.Certificate) string {
	digest := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(digest[:])
}

func localTrustNickname(certificate *x509.Certificate) string {
	return "PaperboatLocal-" + localTrustFingerprint(certificate)
}

func historicalRootsExcept(state string, activePEM []byte) ([]ownedRoot, error) {
	// Without a recorded default domain there is no active CA to require.
	var active *x509.Certificate
	if activePEM != nil {
		var err error
		if active, err = parseLocalTrustRoot(activePEM); err != nil {
			return nil, err
		}
	}
	roots, err := uninstallRoots(state)
	if err != nil {
		return nil, err
	}
	domains, err := loadBrowserDomains(state)
	if err != nil {
		return nil, err
	}
	approved := domains.approved()
	retired := make([]ownedRoot, 0, len(roots))
	foundActive := active == nil
	for _, root := range roots {
		if active != nil && bytes.Equal(root.certificate.Raw, active.Raw) {
			foundActive = true
			continue
		}
		if approved[root.suffix] {
			continue
		}
		retired = append(retired, root)
	}
	if !foundActive {
		return nil, errors.New("active Paperboat local CA is missing from protected state")
	}
	return retired, nil
}

func localTrustDomain(certificate *x509.Certificate) string {
	return strings.TrimPrefix(certificate.PermittedDNSDomains[0], ".")
}
