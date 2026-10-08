// Package testcert supplies isolated TLS fixtures. Production code must not import it.
package testcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

type CA struct {
	Certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func New() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated test issuer"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Certificate: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}
func (c *CA) CertPEM() []byte { return append([]byte(nil), c.pem...) }
func (c *CA) SignCSR(csr *x509.CertificateRequest, expires time.Time) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: expires, DNSNames: csr.DNSNames, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, c.Certificate, csr.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
func (c *CA) IssueCertificate(names []string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	cert, err := c.SignCSR(&x509.CertificateRequest{DNSNames: names, PublicKey: &key.PublicKey}, time.Now().Add(24*time.Hour))
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
func (c *CA) TLS(name string) (tls.Certificate, error) {
	cert, key, err := c.IssueCertificate([]string{name})
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(cert, key)
}
