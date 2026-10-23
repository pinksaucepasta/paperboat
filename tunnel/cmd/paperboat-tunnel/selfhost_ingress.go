package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
)

func installationChallenge(node, host, credential string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.Host
		if h, _, err := net.SplitHostPort(name); err == nil {
			name = h
		}
		if !strings.EqualFold(name, host) || r.URL.Path != "/.well-known/paperboat-installation" {
			next.ServeHTTP(w, r)
			return
		}
		query := r.URL.Query()
		values := query["challenge"]
		if r.Method != "GET" || len(r.URL.RawQuery) > 128 || len(query) != 1 || len(values) != 1 {
			http.Error(w, "invalid challenge", 400)
			return
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
		if err != nil || len(raw) != 32 {
			http.Error(w, "invalid challenge", 400)
			return
		}
		mac := hmac.New(sha256.New, []byte(credential))
		mac.Write([]byte("paperboat-installation/v1\n" + node + "\n" + values[0]))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": node, "challenge": values[0], "proof": base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
	})
}

// Public infrastructure HTTPS uses an operator-provided browser certificate.
// The enrolled Ed25519 certificate is reserved for claim and carrier identity.
// Named publication routes continue to use their own certificates.
func infrastructureTLS(d config.Deployment, base *tls.Config) (*tls.Config, error) {
	if d.PublicHTTPSCertFile == "" && d.PublicHTTPSKeyFile == "" {
		return base, nil
	}
	if d.PublicHTTPSCertFile == "" || d.PublicHTTPSKeyFile == "" {
		return nil, errors.New("public infrastructure HTTPS TLS needs both certificate and key files")
	}
	for _, path := range []string{d.PublicHTTPSCertFile, d.PublicHTTPSKeyFile} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("read public HTTPS TLS files: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, errors.New("public HTTPS TLS files must be bounded regular files")
		}
		if path == d.PublicHTTPSKeyFile && info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("public HTTPS TLS private key requires owner-only permissions")
		}
	}
	certificate, err := tls.LoadX509KeyPair(d.PublicHTTPSCertFile, d.PublicHTTPSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load public infrastructure HTTPS TLS certificate and key: %w", err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse public infrastructure HTTPS TLS certificate: %w", err)
	}
	if err := leaf.VerifyHostname(d.ConnectorAdvertiseHost); err != nil {
		return nil, fmt.Errorf("verify public infrastructure HTTPS TLS hostname: %w", err)
	}
	if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, fmt.Errorf("verify public infrastructure HTTPS TLS validity: %w", x509.CertificateInvalidError{Cert: leaf, Reason: x509.Expired})
	}
	if len(leaf.ExtKeyUsage) > 0 {
		allowed := false
		for _, usage := range leaf.ExtKeyUsage {
			allowed = allowed || usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny
		}
		if !allowed {
			return nil, errors.New("public HTTPS TLS certificate does not permit server authentication")
		}
	}
	result := base.Clone()
	fallback := result.GetCertificate
	result.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName == "" || strings.EqualFold(hello.ServerName, d.ConnectorAdvertiseHost) {
			return &certificate, nil
		}
		return fallback(hello)
	}
	return result, nil
}

// carrierServerTrust binds self-hosted H2/H3 carriers to the identity claimed
// during enrollment. Process epochs and signed admissions fence restarts.
func carrierServerTrust(d config.Deployment, nodeID, epoch, host string, now time.Time) (control.ProcessCarrierServerTrust, error) {
	if d.InfrastructureTLSCertFile == "" && d.InfrastructureTLSKeyFile == "" {
		return control.NewProcessCarrierServerTrust(nodeID, epoch, host, now, control.DefaultProcessCarrierServerCertificateLifetime)
	}
	certificate, err := tls.LoadX509KeyPair(d.InfrastructureTLSCertFile, d.InfrastructureTLSKeyFile)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, fmt.Errorf("load claimed carrier TLS certificate and key: %w", err)
	}
	pin, err := control.CarrierServerSPKISHA256(certificate)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	chain, err := control.CarrierServerCertificateChainPEM(certificate)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	if err = control.ValidateCarrierServerCertificateChain(chain, pin, host, now); err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return control.ProcessCarrierServerTrust{}, fmt.Errorf("verify claimed carrier TLS server-auth usage: %w", err)
	}
	return control.ProcessCarrierServerTrust{Certificate: certificate, SPKISHA256: pin, CertificateChainPEM: chain}, nil
}
