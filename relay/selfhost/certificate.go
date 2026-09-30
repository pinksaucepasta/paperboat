package selfhost

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const certificateRenewalWindow = 30 * 24 * time.Hour
const certificateCheckInterval = 24 * time.Hour

func certificatePEM(key ed25519.PrivateKey, host string, now time.Time) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Paperboat self-host installation"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), nil
}
func writeCertificate(dir string, key ed25519.PrivateKey, host string) error {
	cert, err := certificatePEM(key, host, time.Now())
	if err != nil {
		return err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "tls.crt"), cert)
}

// RenewCertificate validates a replacement with the persistent key before atomic
// publication. Failed preparation leaves the current certificate untouched.
func RenewCertificate(dir string, now time.Time) (bool, error) {
	lock, err := lockState(dir)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	s, err := load(dir)
	if err != nil {
		return false, err
	}
	key, err := privateKey(s)
	if err != nil {
		return false, err
	}
	pair, err := TLSCertificate(dir)
	if err != nil {
		return false, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false, err
	}
	if leaf.NotAfter.Sub(now) > certificateRenewalWindow {
		return false, nil
	}
	oldPin, err := CertificatePin(pair)
	if err != nil {
		return false, err
	}
	newPEM, err := certificatePEM(key, s.Config.EndpointHost, now)
	if err != nil {
		return false, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		return false, err
	}
	next, err := tls.X509KeyPair(newPEM, keyPEM)
	if err != nil {
		return false, err
	}
	pin, err := CertificatePin(next)
	if err != nil {
		return false, err
	}
	parsed, err := x509.ParseCertificate(next.Certificate[0])
	if err != nil || parsed.VerifyHostname(s.Config.EndpointHost) != nil || !now.Before(parsed.NotAfter) || now.Before(parsed.NotBefore) || sha256.Sum256(oldPin) != sha256.Sum256(pin) {
		return false, errors.New("certificate renewal failed identity or validity checks")
	}
	if err := writeFile(filepath.Join(dir, "tls.crt"), newPEM); err != nil {
		return false, err
	}
	return true, nil
}
