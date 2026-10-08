package splitdns

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CA manages a local self-signed root certificate authority for Paperboat.
type CA struct {
	mu        sync.RWMutex
	caCert    *x509.Certificate
	caKey     *rsa.PrivateKey
	caCertPEM []byte
	directory string
}

const (
	leafCertificateLifetime = 30 * 24 * time.Hour
	minimumLeafLifetime     = 24 * time.Hour
	leafBackdate            = time.Hour
)

// LoadOrCreateConstrainedCA owns a root that can sign only names beneath the
// validated private suffix. Existing unconstrained or differently constrained
// roots are rejected and preserved for explicit operator recovery.
func LoadOrCreateConstrainedCA(caDir, suffix string) (*CA, error) {
	clean, err := NormalizeBrowserDomain(suffix)
	if err != nil {
		return nil, err
	}
	return loadOrCreateCA(caDir, clean)
}

// LoadConstrainedCA loads an already provisioned root. Runtime callers cannot
// recreate retired roots, even if deletion races a certificate request.
func LoadConstrainedCA(directory, domain string) (*CA, error) {
	clean, err := NormalizeBrowserDomain(domain)
	if err != nil {
		return nil, err
	}
	certBytes, err := readCAFile(filepath.Join(directory, "rootCA.pem"), 16<<10)
	if err != nil {
		return nil, err
	}
	keyBytes, err := readCAFile(filepath.Join(directory, "rootCA-key.pem"), 16<<10)
	if err != nil {
		return nil, err
	}
	cert, key, err := parseCA(certBytes, keyBytes, time.Now())
	if err != nil {
		return nil, err
	}
	if !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != "."+clean {
		return nil, errors.New("approved CA namespace does not match protected selection")
	}
	if cert.NotAfter.Before(time.Now().Add(minimumLeafLifetime)) {
		return nil, errors.New("approved CA expires too soon; reinstall Paperboat to renew local HTTPS trust")
	}
	return &CA{caCert: cert, caKey: key, caCertPEM: certBytes, directory: directory}, nil
}

func loadOrCreateCA(caDir, suffix string) (*CA, error) {
	if err := os.MkdirAll(caDir, 0700); err != nil {
		return nil, fmt.Errorf("create ca dir %s: %w", caDir, err)
	}

	certPath := filepath.Join(caDir, "rootCA.pem")
	keyPath := filepath.Join(caDir, "rootCA-key.pem")

	ca := &CA{directory: caDir}

	certBytes, errCert := readCAFile(certPath, 16<<10)
	keyBytes, errKey := readCAFile(keyPath, 16<<10)

	if errCert == nil && errKey == nil {
		now := time.Now()
		cert, key, err := parseCA(certBytes, keyBytes, now)
		if err != nil {
			return nil, fmt.Errorf("load existing Paperboat CA (reinstall Paperboat for scoped local HTTPS trust repair (state retained at %s and %s)): %w", certPath, keyPath, err)
		}
		if cert.NotAfter.Before(now.Add(minimumLeafLifetime)) {
			return nil, fmt.Errorf("existing Paperboat CA expires at %s and cannot issue a certificate valid for 24 hours; reinstall Paperboat to renew local HTTPS trust (state retained at %s and %s)", cert.NotAfter.UTC().Format(time.RFC3339), certPath, keyPath)
		}
		if suffix != "" && (!cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != "."+suffix) {
			return nil, errors.New("existing Paperboat CA is not constrained to the active private suffix")
		}
		ca.caCert, ca.caKey, ca.caCertPEM = cert, key, certBytes
		return ca, nil
	}
	if errCert == nil || errKey == nil || (!os.IsNotExist(errCert) && errCert != nil) || (!os.IsNotExist(errKey) && errKey != nil) {
		return nil, errors.New("Paperboat CA is incomplete or unreadable; existing trust material was preserved")
	}

	// Generate new root CA
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate ca key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Paperboat Local Development CA"},
			CommonName:   "Paperboat Local Root CA",
			Country:      []string{"US"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0), // 10 years
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	if suffix != "" {
		caTemplate.PermittedDNSDomainsCritical = true
		caTemplate.PermittedDNSDomains = []string{"." + suffix}
	}

	caDer, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &privKey.PublicKey, privKey)
	if err != nil {
		return nil, fmt.Errorf("create ca cert: %w", err)
	}

	parsedCert, err := x509.ParseCertificate(caDer)
	if err != nil {
		return nil, fmt.Errorf("parse generated ca cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDer})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)})

	if err := atomicWrite(keyPath, keyPEM, 0600); err != nil {
		_ = os.Remove(keyPath)
		return nil, fmt.Errorf("write rootCA-key.pem: %w", err)
	}
	if err := atomicWrite(certPath, certPEM, 0644); err != nil {
		// Both paths were absent when creation began, so neither file has been
		// exposed as trusted state yet. Remove the new key and any renamed cert
		// so the next start can retry the transaction without replacing trust.
		_ = os.Remove(keyPath)
		_ = os.Remove(certPath)
		return nil, fmt.Errorf("write rootCA.pem: %w", err)
	}

	ca.caCert = parsedCert
	ca.caKey = privKey
	ca.caCertPEM = certPEM

	return ca, nil
}

// CertPEM returns the PEM-encoded root CA certificate.
func (c *CA) CertPEM() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]byte(nil), c.caCertPEM...)
}

// IssueCertificate issues a dynamic TLS leaf certificate for domain patterns (e.g. "*.homelab.pprbt", "homelab.pprbt").
func (c *CA) IssueCertificate(domains []string) ([]byte, []byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(domains) == 0 || len(domains) > 8 {
		return nil, nil, errors.New("certificate requires 1 to 8 bounded names")
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf serial: %w", err)
	}

	if !c.caCert.PermittedDNSDomainsCritical || len(c.caCert.PermittedDNSDomains) != 1 {
		return nil, nil, errors.New("leaf issuance requires a constrained browser CA")
	}
	domain := strings.TrimPrefix(c.caCert.PermittedDNSDomains[0], ".")
	if clean, err := NormalizeBrowserDomain(domain); err != nil || clean != domain {
		return nil, nil, errors.New("invalid browser CA constraint")
	}
	var dnsNames []string
	var ipAddresses []net.IP

	for _, d := range domains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" || len(d) > 253 || strings.ContainsAny(d, "/:@") {
			return nil, nil, errors.New("invalid certificate name")
		}
		if net.ParseIP(d) != nil || !strings.HasSuffix(d, "."+domain) || strings.Contains(strings.TrimPrefix(d, "*."), "*") {
			return nil, nil, errors.New("certificate name is outside local browser namespace")
		}
		for _, label := range strings.Split(strings.TrimPrefix(d, "*."), ".") {
			if !validLabel(label) {
				return nil, nil, errors.New("invalid browser certificate label")
			}
		}
		dnsNames = append(dnsNames, d)
	}

	commonName := "localhost"
	if len(dnsNames) > 0 {
		commonName = dnsNames[0]
	}

	now := time.Now()
	if c.caCert.NotAfter.Before(now.Add(minimumLeafLifetime)) {
		return nil, nil, fmt.Errorf("Paperboat root CA expires at %s and cannot issue a certificate valid for 24 hours; reinstall Paperboat to renew local HTTPS trust", c.caCert.NotAfter.UTC().Format(time.RFC3339))
	}
	notBefore := now.Add(-leafBackdate)
	if c.caCert.NotBefore.After(notBefore) {
		notBefore = c.caCert.NotBefore
	}
	notAfter := now.Add(leafCertificateLifetime)
	if c.caCert.NotAfter.Before(notAfter) {
		notAfter = c.caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Paperboat Domain"},
			CommonName:   commonName,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
		BasicConstraintsValid: true,
		CRLDistributionPoints: []string{"http://" + BrowserGatewayIP + CRLPath(c.caCert.Raw)},
	}

	leafDer, err := x509.CreateCertificate(rand.Reader, template, c.caCert, &leafKey.PublicKey, c.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create leaf certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDer})
	// Bundle root CA
	certPEM = append(certPEM, c.caCertPEM...)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})

	return certPEM, keyPEM, nil
}

func parseCA(certPEM, keyPEM []byte, now time.Time) (*x509.Certificate, *rsa.PrivateKey, error) {
	certBlock, certRest := pem.Decode(certPEM)
	keyBlock, keyRest := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil || len(strings.TrimSpace(string(certRest))) != 0 || len(strings.TrimSpace(string(keyRest))) != 0 {
		return nil, nil, errors.New("invalid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.CheckSignatureFrom(cert) != nil || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.KeyUsage&x509.KeyUsageCRLSign == 0 || cert.PublicKeyAlgorithm != x509.RSA || cert.PublicKey.(*rsa.PublicKey).N.Cmp(key.PublicKey.N) != 0 || cert.PublicKey.(*rsa.PublicKey).E != key.PublicKey.E {
		return nil, nil, errors.New("certificate and key are not a valid matching CA")
	}
	if now.Before(cert.NotBefore) {
		return nil, nil, fmt.Errorf("CA is not valid until %s", cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.Before(cert.NotAfter) {
		return nil, nil, fmt.Errorf("CA expired at %s", cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return cert, key, nil
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	return writeCAState(path, content, mode)
}
func readCAFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("unsafe or oversized local CA state")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("local CA file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if len(data) > int(limit) {
		return nil, errors.New("oversized local CA state")
	}
	return data, err
}

func (c *CA) ExpiresAt() time.Time { c.mu.RLock(); defer c.mu.RUnlock(); return c.caCert.NotAfter }
