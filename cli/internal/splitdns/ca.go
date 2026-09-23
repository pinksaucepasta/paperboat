package splitdns

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
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
	clean, err := ValidateSuffix(suffix)
	if err != nil {
		return nil, err
	}
	return loadOrCreateCA(caDir, clean)
}

func loadOrCreateCA(caDir, suffix string) (*CA, error) {
	if err := os.MkdirAll(caDir, 0700); err != nil {
		return nil, fmt.Errorf("create ca dir %s: %w", caDir, err)
	}

	certPath := filepath.Join(caDir, "rootCA.pem")
	keyPath := filepath.Join(caDir, "rootCA-key.pem")

	ca := &CA{}

	certBytes, errCert := os.ReadFile(certPath)
	keyBytes, errKey := os.ReadFile(keyPath)

	if errCert == nil && errKey == nil {
		now := time.Now()
		cert, key, err := parseCA(certBytes, keyBytes, now)
		if err != nil {
			return nil, fmt.Errorf("load existing Paperboat CA (remove its installed trust, then delete both %s and %s for explicit recovery): %w", certPath, keyPath, err)
		}
		if cert.NotAfter.Before(now.Add(minimumLeafLifetime)) {
			return nil, fmt.Errorf("existing Paperboat CA expires at %s and cannot issue a certificate valid for 24 hours; remove its installed trust, then delete both %s and %s before restarting to create a new root", cert.NotAfter.UTC().Format(time.RFC3339), certPath, keyPath)
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
	return c.caCertPEM
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

	var dnsNames []string
	var ipAddresses []net.IP

	for _, d := range domains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" || len(d) > 253 || strings.ContainsAny(d, "/:@") {
			return nil, nil, errors.New("invalid certificate name")
		}
		if ip := net.ParseIP(d); ip != nil {
			ipAddresses = append(ipAddresses, ip)
		} else {
			dnsNames = append(dnsNames, d)
		}
	}

	commonName := "localhost"
	if len(dnsNames) > 0 {
		commonName = dnsNames[0]
	}

	now := time.Now()
	if c.caCert.NotAfter.Before(now.Add(minimumLeafLifetime)) {
		return nil, nil, fmt.Errorf("Paperboat root CA expires at %s and cannot issue a certificate valid for 24 hours; remove installed trust and recreate both root certificate and key", c.caCert.NotAfter.UTC().Format(time.RFC3339))
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
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
		BasicConstraintsValid: true,
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
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.PublicKeyAlgorithm != x509.RSA || cert.PublicKey.(*rsa.PublicKey).N.Cmp(key.PublicKey.N) != 0 || cert.PublicKey.(*rsa.PublicKey).E != key.PublicKey.E {
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".paperboat-ca-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(content)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceFile(name, path)
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
